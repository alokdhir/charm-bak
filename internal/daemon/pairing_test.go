package daemon

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"charm-bak/internal/protocol"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// The whole pairing: pair_start's command fetches a script that names
// this daemon's pin, the binary is served for the remote's arch, and the
// remote's pairing call pins both sides. The code works once.
func TestPairing(t *testing.T) {
	dir := t.TempDir()
	macID, _ := LoadIdentity(filepath.Join(dir, "mac.pem"), "studio")
	pantryID, _ := LoadIdentity(filepath.Join(dir, "pantry.pem"), "pantry")
	bin := filepath.Join(dir, "linux-amd64", "charm-bak-daemon")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("ELF fake daemon"), 0o755)

	macStore := memStore(t)
	mac := NewCore("test", macStore, func(RepositoryConfig) Engine { return &statsEngine{} }, nil)
	mac.Name = "studio"
	mac.pairPortOverride = freePort(t)
	mac.SetRemoteDaemons(dir)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mac.StartRemotes(ctx, macID)
	c, _ := serve(t, mac)

	if code := callErr(c, protocol.MethodPairStart, protocol.PairStartParams{Name: "Bad Name!"}); code != protocol.ErrBadRequest {
		t.Errorf("bad name: %s", code)
	}
	res := call[protocol.PairStartResult](t, c, protocol.MethodPairStart, protocol.PairStartParams{Name: "pantry"})
	m := regexp.MustCompile(`--pinnedpubkey '([^']+)' -k https://([^/]+)/i/(\w+) \| sudo sh$`).FindStringSubmatch(res.Command)
	if m == nil || m[1] != macID.Pin {
		t.Fatalf("command %q", res.Command)
	}
	_, port, _ := net.SplitHostPort(m[2])
	base := "https://127.0.0.1:" + port
	code := m[3]

	if curl, err := exec.LookPath("curl"); err == nil {
		// curl enforces the pin even with -k: a wrong pin fails.
		wrong := exec.Command(curl, "-fsS", "--pinnedpubkey", pantryID.Pin, "-k", base+"/i/"+code)
		if out, err := wrong.CombinedOutput(); err == nil {
			t.Errorf("curl accepted the wrong pin: %s", out)
		}
		script, err := exec.Command(curl, "-fsS", "--pinnedpubkey", macID.Pin, "-k", base+"/i/"+code).Output()
		if err != nil {
			t.Fatalf("curl with the right pin: %v", err)
		}
		for _, want := range []string{"-name pantry", "-controller-pin '" + macID.Pin + "'", "/d/" + code + "/$arch", "-listen :7457", "systemctl restart charm-bak.service"} {
			if !strings.Contains(string(script), want) {
				t.Errorf("script lacks %q", want)
			}
		}
		got, err := exec.Command(curl, "-fsS", "--pinnedpubkey", macID.Pin, "-k", base+"/d/"+code+"/amd64").Output()
		if err != nil || string(got) != "ELF fake daemon" {
			t.Errorf("binary: %q %v", got, err)
		}
	} else {
		t.Log("no curl: script and binary fetches not checked")
	}

	pantryStore := memStore(t)
	if _, err := PairWith(context.Background(), pantryStore, pantryID, "pantry", base+"/p/"+code, pantryID.Pin); err == nil {
		t.Error("paired with a controller under the wrong pin")
	}
	ctrl, err := PairWith(context.Background(), pantryStore, pantryID, "pantry", base+"/p/"+code, macID.Pin)
	if err != nil || ctrl != "studio" {
		t.Fatalf("pair: %q %v", ctrl, err)
	}
	peers, _ := macStore.Peers()
	if len(peers) != 1 || peers[0].Name != "pantry" || peers[0].Pin != pantryID.Pin || peers[0].Address != "127.0.0.1:7457" {
		t.Errorf("mac's peers %+v", peers)
	}
	theirs, _ := pantryStore.Peers()
	if len(theirs) != 1 || theirs[0].Name != "studio" || theirs[0].Address != "" || theirs[0].Pin != macID.Pin {
		t.Errorf("pantry's peers %+v", theirs)
	}
	if hosts := call[protocol.HostsResult](t, c, protocol.MethodHosts, nil); len(hosts.Hosts) != 2 {
		t.Errorf("hosts after pairing: %+v", hosts.Hosts)
	}
	if _, err := PairWith(context.Background(), memStore(t), pantryID, "pantry", base+"/p/"+code, macID.Pin); err == nil {
		t.Error("the code worked twice")
	}

	// Repair: the same install, for a paired remote, without pairing.
	if code := callErr(c, protocol.MethodHostRepair, protocol.HostRepairParams{Name: "wren"}); code != protocol.ErrNotFound {
		t.Errorf("repair of an unknown host: %s", code)
	}
	rep := call[protocol.PairStartResult](t, c, protocol.MethodHostRepair, protocol.HostRepairParams{Name: "pantry"})
	m = regexp.MustCompile(`/i/(\w+) \| sudo sh$`).FindStringSubmatch(rep.Command)
	if m == nil {
		t.Fatalf("repair command %q", rep.Command)
	}
	if curl, err := exec.LookPath("curl"); err == nil {
		script, err := exec.Command(curl, "-fsS", "--pinnedpubkey", macID.Pin, "-k", base+"/i/"+m[1]).Output()
		if err != nil {
			t.Fatalf("repair script: %v", err)
		}
		if strings.Contains(string(script), "-pair ") || !strings.Contains(string(script), "reinstalled") || !strings.Contains(string(script), "/d/"+m[1]+"/$arch") {
			t.Errorf("repair script:\n%s", script)
		}
	}
	if _, err := PairWith(context.Background(), memStore(t), pantryID, "pantry", base+"/p/"+m[1], macID.Pin); err == nil {
		t.Error("a repair code paired")
	}
}

// The install script parses as sh, both branches (Linux and Mac).
func TestInstallScriptParses(t *testing.T) {
	for _, repair := range []bool{false, true} {
		var b bytes.Buffer
		if err := installScript.Execute(&b, map[string]any{"Controller": "studio", "Name": "wren", "Pin": "sha256//x",
			"Code": "CODE", "Addr": "192.168.1.2:7458", "RemotePort": RemotePort, "MacLabel": MacLabel, "Repair": repair}); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = &b
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("repair %v: sh -n: %v %s", repair, err, out)
		}
	}
}

// The Mac bundle download is a tar.gz of charm-bak.app as built.
func TestMacBundleTar(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "darwin-arm64", "charm-bak.app", "Contents", "MacOS", "charm-bak-daemon")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.WriteFile(exe, []byte("bin"), 0o755)
	var b bytes.Buffer
	if err := tarGz(&b, filepath.Join(dir, "darwin-arm64", "charm-bak.app")); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	cmd := exec.Command("tar", "-xzf", "-", "-C", out)
	cmd.Stdin = &b
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, o)
	}
	fi, err := os.Stat(filepath.Join(out, "charm-bak.app", "Contents", "MacOS", "charm-bak-daemon"))
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("extracted: %v %v", fi, err)
	}
}
