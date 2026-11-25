package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"charm-bak/internal/protocol"
)

// Pairing a remote (docs/design.md, "Revision 2026-11-19"): pair_start
// makes a one-time code and serves, for pairTTL on PairPort, an install
// script, the Linux daemon and a pairing endpoint, all over TLS with this
// daemon's identity. The user pastes one command on the remote as root;
// curl checks this daemon's key pin, the script installs the daemon as a
// systemd service, and the new daemon presents its own key with the code.
// Each side then pins the other. The code works once.

// PairPort is where a controller serves pairing.
const PairPort = 7458

const pairTTL = 10 * time.Minute

var hostNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

type pairing struct {
	mu      sync.Mutex
	code    string
	name    string
	repair  bool // reinstalls a paired remote; nothing to pair
	expires time.Time
	srv     *http.Server
}

// LinuxDaemons is the folder holding linux-amd64/charm-bak-daemon and
// linux-arm64/charm-bak-daemon, which pairing hands to remotes.
func (c *Core) SetRemoteDaemons(dir string) { c.remoteDaemons = dir }

// startPairing handles pair_start.
func (c *Core) startPairing(p protocol.PairStartParams) (protocol.PairStartResult, error) {
	name := strings.ToLower(strings.TrimSpace(p.Name))
	if !hostNameRE.MatchString(name) {
		return protocol.PairStartResult{}, perr(protocol.ErrBadRequest, "A host name is lowercase letters, digits and dashes, like pantry.")
	}
	if name == c.Name {
		return protocol.PairStartResult{}, perr(protocol.ErrBadRequest, "%s is this machine.", name)
	}
	return c.serveInstall(name, false)
}

// startRepair handles host_repair: the install command again for a
// paired remote, minus the pairing. Run there, it puts this app's daemon
// back with its service (launchd plist or systemd unit) and leaves its
// state alone: database, identity key, secrets, pairing.
func (c *Core) startRepair(p protocol.HostRepairParams) (protocol.PairStartResult, error) {
	c.remotes.mu.Lock()
	r := c.remotes.byID[p.Name]
	c.remotes.mu.Unlock()
	if r == nil {
		return protocol.PairStartResult{}, perr(protocol.ErrNotFound, "%s isn't a paired remote.", p.Name)
	}
	return c.serveInstall(p.Name, true)
}

// serveInstall makes a one-time code and serves the install script for
// name (repair: without pairing) for pairTTL.
func (c *Core) serveInstall(name string, repair bool) (protocol.PairStartResult, error) {
	if c.identity == nil {
		return protocol.PairStartResult{}, perr(protocol.ErrInternal, "This daemon has no identity key loaded.")
	}
	addr, err := lanAddress()
	if err != nil {
		return protocol.PairStartResult{}, perr(protocol.ErrInternal, "Couldn't find this machine's network address: %v", err)
	}
	code := newCode()

	c.pair.mu.Lock()
	defer c.pair.mu.Unlock()
	if c.pair.srv != nil { // a new pairing replaces one in progress
		c.pair.srv.Close()
	}
	ln, err := tls.Listen("tcp", fmt.Sprintf(":%d", c.pairPort()), &tls.Config{
		MinVersion:   tls.VersionTLS12, // curl on older remotes
		Certificates: []tls.Certificate{c.identity.Cert},
		ClientAuth:   tls.RequestClientCert, // the pairing call presents the remote's key
	})
	if err != nil {
		return protocol.PairStartResult{}, perr(protocol.ErrInternal, "Couldn't listen for pairing on port %d: %v", c.pairPort(), err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /i/{code}", c.pairScript(addr))
	mux.HandleFunc("GET /d/{code}/{arch}", c.pairBinary)
	mux.HandleFunc("GET /m/{code}/{arch}", c.pairMacBundle)
	mux.HandleFunc("GET /w/{code}", c.pairWindowsScript(addr))
	mux.HandleFunc("GET /x/{code}/{arch}", c.pairWindowsBinary)
	mux.HandleFunc("POST /p/{code}", c.pairComplete)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	c.pair.srv, c.pair.code, c.pair.name, c.pair.repair, c.pair.expires = srv, code, name, repair, c.now().Add(pairTTL)
	go srv.Serve(ln)
	time.AfterFunc(pairTTL, func() { c.endPairing(srv) })

	hostport := net.JoinHostPort(addr, strconv.Itoa(c.pairPort()))
	cmd := fmt.Sprintf("curl -fsS --pinnedpubkey '%s' -k https://%s/i/%s | sudo sh", c.identity.Pin, hostport, code)
	what := "pairing"
	if repair {
		what = "repair"
	}
	logf("%s started for %s on %s, code expires %s", what, name, hostport, c.pair.expires.Local().Format("15:04"))
	return protocol.PairStartResult{Command: cmd, WindowsCommand: windowsCommand(c.identity.Pin, hostport, code), Expires: c.pair.expires}, nil
}

func (c *Core) pairPort() int {
	if c.pairPortOverride != 0 {
		return c.pairPortOverride
	}
	return PairPort
}

// endPairing stops srv if it's still the current pairing.
func (c *Core) endPairing(srv *http.Server) {
	c.pair.mu.Lock()
	defer c.pair.mu.Unlock()
	if c.pair.srv == srv {
		srv.Close()
		c.pair.srv, c.pair.code = nil, ""
	}
}

// validCode says whether code is the live pairing's.
func (c *Core) validCode(code string) (name string, ok bool) {
	name, _, ok = c.validCodeFor(code)
	return name, ok
}

// validCodeFor is validCode, plus whether the code is for a repair.
func (c *Core) validCodeFor(code string) (name string, repair, ok bool) {
	c.pair.mu.Lock()
	defer c.pair.mu.Unlock()
	if c.pair.code == "" || code != c.pair.code || c.now().After(c.pair.expires) {
		return "", false, false
	}
	return c.pair.name, c.pair.repair, true
}

func newCode() string {
	b := make([]byte, 10)
	rand.Read(b)
	return base32.StdEncoding.EncodeToString(b)
}

// lanAddress is the address remotes can reach this machine on: the
// source address of the default route (dialing UDP sends nothing).
func lanAddress() (string, error) {
	conn, err := net.Dial("udp", "192.0.2.1:9") // TEST-NET-1: routed like the internet, never answered
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// installScript runs on the remote, as root, piped from curl.
var installScript = template.Must(template.New("install").Parse(`#!/bin/sh
{{if .Repair}}# charm-bak: reinstall this machine's backup daemon (controlled by {{.Controller}}),
# keeping its state: database, keys, pairing.{{else}}# charm-bak: install this machine's backup daemon and pair it with {{.Controller}}.{{end}}
set -eu
[ "$(id -u)" = 0 ] || { echo "charm-bak: run this as root (... | sudo sh)" >&2; exit 1; }
if [ "$(uname -s)" = Darwin ]; then
  # A Mac: the daemon in its own small app bundle (Full Disk Access is
  # granted to an app), started by launchd as root.
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    arm64) arch=arm64 ;;
    *) echo "charm-bak: no daemon for $(uname -m)" >&2; exit 1 ;;
  esac
  [ -x /opt/homebrew/bin/restic ] || [ -x /usr/local/bin/restic ] || echo "charm-bak: restic isn't installed; backups need it (brew install restic)" >&2
  dir="/Library/Application Support/charm-bak"
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  curl -fsS --pinnedpubkey '{{.Pin}}' -k "https://{{.Addr}}/m/{{.Code}}/$arch" -o "$tmp/app.tgz"
  launchctl bootout system/{{.MacLabel}} 2>/dev/null || true
  install -d -m 0755 "$dir"
  install -d -m 0700 "$dir/state"
  rm -rf "$dir/charm-bak.app"
  tar -xzf "$tmp/app.tgz" -C "$dir"
  chown -R root:wheel "$dir/charm-bak.app" # tar keeps the build's owner: root runs this
  exe="$dir/charm-bak.app/Contents/MacOS/charm-bak-daemon"
{{if not .Repair}}  "$exe" -name {{.Name}} -db "$dir/state/charm-bak.db" -identity "$dir/state/identity.pem" -pair "https://{{.Addr}}/p/{{.Code}}" -controller-pin '{{.Pin}}'
{{end}}  cat > /Library/LaunchDaemons/{{.MacLabel}}.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>{{.MacLabel}}</string>
  <key>AssociatedBundleIdentifiers</key><array><string>{{.MacLabel}}.remote</string></array>
  <key>ProgramArguments</key>
  <array>
    <string>$exe</string>
    <string>-name</string><string>{{.Name}}</string>
    <string>-listen</string><string>:{{.RemotePort}}</string>
    <string>-db</string><string>$dir/state/charm-bak.db</string>
    <string>-identity</string><string>$dir/state/identity.pem</string>
    <string>-socket</string><string>/var/run/charm-bak/daemon.sock</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardErrorPath</key><string>$dir/state/daemon.log</string>
</dict>
</plist>
PLIST
  chmod 0644 /Library/LaunchDaemons/{{.MacLabel}}.plist
  launchctl bootstrap system /Library/LaunchDaemons/{{.MacLabel}}.plist
{{if .Repair}}  echo "charm-bak: {{.Name}}'s daemon is reinstalled and running."{{else}}  echo "charm-bak: {{.Name}} is paired with {{.Controller}} and running. Next: Full Disk Access, which charm-bak on {{.Controller}} walks you through."{{end}}
  exit 0
fi

command -v systemctl >/dev/null || { echo "charm-bak: this needs systemd" >&2; exit 1; }
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "charm-bak: no daemon for $(uname -m)" >&2; exit 1 ;;
esac
command -v restic >/dev/null || echo "charm-bak: restic isn't installed; backups need it (e.g. apt install restic)" >&2

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
curl -fsS --pinnedpubkey '{{.Pin}}' -k "https://{{.Addr}}/d/{{.Code}}/$arch" -o "$tmp"
install -m 0755 "$tmp" /usr/local/bin/charm-bak-daemon
install -d -m 0700 /var/lib/charm-bak

{{if not .Repair}}/usr/local/bin/charm-bak-daemon -name {{.Name}} -pair "https://{{.Addr}}/p/{{.Code}}" -controller-pin '{{.Pin}}'
{{end}}
cat > /etc/systemd/system/charm-bak.service <<'UNIT'
[Unit]
Description=charm-bak backup daemon (controlled by {{.Controller}})
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/charm-bak-daemon -name {{.Name}} -listen :{{.RemotePort}}
Restart=on-failure
RestartSec=5
StateDirectory=charm-bak
StateDirectoryMode=0700
RuntimeDirectory=charm-bak
RuntimeDirectoryMode=0700
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable charm-bak.service
systemctl restart charm-bak.service
{{if .Repair}}echo "charm-bak: {{.Name}}'s daemon is reinstalled and running (systemctl status charm-bak)."{{else}}echo "charm-bak: {{.Name}} is paired with {{.Controller}} and running (systemctl status charm-bak)."{{end}}
`))

func (c *Core) pairScript(addr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, repair, ok := c.validCodeFor(r.PathValue("code"))
		if !ok {
			http.Error(w, "echo 'charm-bak: that code has expired; start again in charm-bak.' >&2; exit 1", http.StatusNotFound)
			return
		}
		var b bytes.Buffer
		installScript.Execute(&b, map[string]any{
			"Controller": c.Name, "Name": name, "Pin": c.identity.Pin, "Code": r.PathValue("code"),
			"Addr": net.JoinHostPort(addr, strconv.Itoa(c.pairPort())), "RemotePort": RemotePort, "MacLabel": MacLabel, "Repair": repair,
		})
		w.Header().Set("Content-Type", "text/x-shellscript")
		w.Write(b.Bytes())
	}
}

func (c *Core) pairBinary(w http.ResponseWriter, r *http.Request) {
	if _, ok := c.validCode(r.PathValue("code")); !ok {
		http.Error(w, "expired", http.StatusNotFound)
		return
	}
	arch := r.PathValue("arch")
	if arch != "amd64" && arch != "arm64" {
		http.Error(w, "no such build", http.StatusNotFound)
		return
	}
	path := c.remoteDaemon("linux", arch)
	if _, err := os.Stat(path); err != nil {
		http.Error(w, "this app has no linux-"+arch+" daemon", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, path)
}

// MacLabel is a Mac remote's launchd job (main.go's macLabel).
const MacLabel = "io.github.alokdhir.charm-bak"

// pairMacBundle serves a Mac remote's daemon bundle as a tar.gz of
// charm-bak.app, modes and code signature as built.
func (c *Core) pairMacBundle(w http.ResponseWriter, r *http.Request) {
	if _, ok := c.validCode(r.PathValue("code")); !ok {
		http.Error(w, "expired", http.StatusNotFound)
		return
	}
	exe := c.remoteDaemon("darwin", r.PathValue("arch"))
	if exe == "" {
		http.Error(w, "no such build", http.StatusNotFound)
		return
	}
	bundle := filepath.Dir(filepath.Dir(filepath.Dir(exe))) // …/charm-bak.app
	if _, err := os.Stat(exe); err != nil {
		http.Error(w, "this app has no Mac daemon for "+r.PathValue("arch"), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	if err := tarGz(w, bundle); err != nil {
		logf("serving the Mac daemon: %v", err)
	}
}

// tarGz writes dir (named by its base) as a gzipped tar.
func tarGz(w io.Writer, dir string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	base := filepath.Dir(dir)
	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, path)
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// pairRequest and pairReply are the pairing call's bodies.
type pairRequest struct {
	Name string `json:"name"`
}

type pairReply struct {
	Controller string `json:"controller"`
}

// pairComplete is the remote's pairing call: its key comes in the TLS
// handshake, the code in the URL. Success pins it as a remote at the
// address it called from.
func (c *Core) pairComplete(w http.ResponseWriter, r *http.Request) {
	name, repair, ok := c.validCodeFor(r.PathValue("code"))
	if !ok || repair {
		http.Error(w, "that pairing code has expired or was used", http.StatusNotFound)
		return
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "present the daemon's key", http.StatusUnauthorized)
		return
	}
	var req pairRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Name != name {
		http.Error(w, "this code is for "+name, http.StatusBadRequest)
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	p := Peer{Name: name, Address: net.JoinHostPort(host, strconv.Itoa(RemotePort)), Pin: pinOf(r.TLS.PeerCertificates[0]), Added: c.now()}
	if err := c.store.AddPeer(p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logf("paired with remote %s at %s (key %s)", p.Name, p.Address, p.Pin)
	c.pair.mu.Lock()
	c.pair.code = "" // used
	srv := c.pair.srv
	c.pair.mu.Unlock()
	c.followNew(p)
	json.NewEncoder(w).Encode(pairReply{Controller: c.Name})
	// Let the script's last requests finish, then stop serving.
	time.AfterFunc(5*time.Second, func() { c.endPairing(srv) })
}

// PairWith is the remote's side, run by the install script: present this
// daemon's key and the code to the controller at url (checking its pin),
// then accept that controller.
func PairWith(ctx context.Context, store *Store, id *Identity, name, url, controllerPin string) (string, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		Certificates:       []tls.Certificate{id.Cert},
		InsecureSkipVerify: true, // the pin is the check
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			pin, err := peerPin(raw)
			if err != nil {
				return err
			}
			if pin != controllerPin {
				return fmt.Errorf("the controller presented %s, not the pinned %s", pin, controllerPin)
			}
			return nil
		},
	}
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	body, _ := json.Marshal(pairRequest{Name: name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("pairing refused: %s", strings.TrimSpace(string(msg)))
	}
	var rep pairReply
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil || rep.Controller == "" {
		return "", errors.New("pairing: the controller's reply made no sense")
	}
	if err := store.AddPeer(Peer{Name: rep.Controller, Pin: controllerPin, Added: time.Now()}); err != nil {
		return "", err
	}
	return rep.Controller, nil
}
