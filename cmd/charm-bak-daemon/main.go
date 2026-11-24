// Command charm-bak-daemon runs one machine's backups: it keeps the
// sources, schedule and history, runs restic, and serves the TUI and the
// menu bar app on a Unix socket (docs/protocol.md). On the Mac the menu
// bar app starts it; on Linux, systemd does.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"charm-bak/internal/daemon"
)

const version = "0.3.0"

// selfPath is this executable's path, read at startup: after an update
// renames a new file over it, Linux reports the running one as deleted.
var selfPath string

func main() {
	if restart := run(); restart {
		// self_update put a new executable in place: become it, same
		// arguments, same process (so whatever supervises this one sees
		// no exit). Only if that fails, exit non-zero, so a service
		// manager that restarts on failure starts the new one.
		reexec()
		os.Exit(75)
	}
}

// run is the daemon; it returns true when it shut down to restart into
// an updated executable.
func run() (restart bool) {
	uninstall := false
	socket := flag.String("socket", daemon.DefaultSocketPath(), "Unix socket to serve on")
	db := flag.String("db", daemon.DefaultStorePath(), "the daemon's database")
	env := flag.Bool("env", false, `print the repository as shell exports and exit, for running restic by hand: eval "$(charm-bak-daemon -env)"; secrets are read from the Keychain when the exports run, never printed`)
	exports := flag.Bool("exports", false, "read a stored credential set on stdin and print it as shell exports (what -env's eval line runs)")
	withParent := flag.Bool("exit-with-parent", false, "shut down when stdin closes: the menu bar app holds the other end, so the daemon never outlives it, however the app ends")
	name := flag.String("name", daemon.HostName(), "this machine's name, as paired daemons and the TUI know it")
	identity := flag.String("identity", daemon.DefaultIdentityPath(), "this daemon's key for pairing (created if missing)")
	listen := flag.String("listen", "", fmt.Sprintf("serve paired controllers over mutual TLS on this address (a remote: \":%d\")", daemon.RemotePort))
	showPin := flag.Bool("pin", false, "print this daemon's key pin and exit")
	addRemote := flag.String("add-remote", "", "pair with a remote and exit: name,host:port,pin")
	addController := flag.String("add-controller", "", "accept a controller and exit: name,pin")
	removePeer := flag.String("remove-peer", "", "unpair a remote or controller by name and exit")
	listPeers := flag.Bool("peers", false, "list paired daemons and exit")
	pairURL := flag.String("pair", "", "pair with the controller at this URL and exit (the install script runs this, with -controller-pin)")
	controllerPin := flag.String("controller-pin", "", "with -pair: the controller's key pin")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*db), 0o700); err != nil {
		fail(err)
	}
	store, err := daemon.OpenStore(*db)
	if err != nil {
		fail(err)
	}
	defer store.Close()

	id, err := daemon.LoadIdentity(*identity, *name)
	if err != nil {
		fail(err)
	}
	switch {
	case *showPin:
		fmt.Println(id.Pin)
		return
	case *addRemote != "":
		f := strings.Split(*addRemote, ",")
		if len(f) != 3 || !strings.HasPrefix(f[2], "sha256//") {
			fail(fmt.Errorf("-add-remote wants name,host:port,sha256//pin"))
		}
		if err := store.AddPeer(daemon.Peer{Name: f[0], Address: f[1], Pin: f[2], Added: time.Now()}); err != nil {
			fail(err)
		}
		fmt.Println("paired with remote", f[0])
		return
	case *addController != "":
		f := strings.Split(*addController, ",")
		if len(f) != 2 || !strings.HasPrefix(f[1], "sha256//") {
			fail(fmt.Errorf("-add-controller wants name,sha256//pin"))
		}
		if err := store.AddPeer(daemon.Peer{Name: f[0], Pin: f[1], Added: time.Now()}); err != nil {
			fail(err)
		}
		fmt.Println("accepting controller", f[0])
		return
	case *removePeer != "":
		if err := store.RemovePeer(*removePeer); err != nil {
			fail(err)
		}
		fmt.Println("unpaired", *removePeer)
		return
	case *pairURL != "":
		ctrl, err := daemon.PairWith(context.Background(), store, id, *name, *pairURL, *controllerPin)
		if err != nil {
			fail(err)
		}
		fmt.Println("paired with", ctrl)
		return
	case *listPeers:
		peers, err := store.Peers()
		if err != nil {
			fail(err)
		}
		for _, p := range peers {
			role := "remote " + p.Address
			if p.Address == "" {
				role = "controller"
			}
			fmt.Printf("%s\t%s\t%s\n", p.Name, role, p.Pin)
		}
		return
	}

	if *exports {
		in, err := io.ReadAll(os.Stdin)
		if err != nil {
			fail(err)
		}
		out, err := daemon.Exports(string(in))
		if err != nil {
			fail(err)
		}
		fmt.Print(out)
		return
	}

	if *env {
		repo, err := store.Repository()
		if err != nil {
			fail(err)
		}
		if repo == nil {
			fail(fmt.Errorf("no repository is set up"))
		}
		out, err := daemon.ShellEnv(*repo)
		if err != nil {
			fail(err)
		}
		fmt.Print(out)
		return
	}

	if err := daemon.SetupLog(filepath.Join(filepath.Dir(*db), "daemon.log")); err != nil {
		fail(err)
	}
	if os.Getenv("HOME") == "" && os.Getenv("RESTIC_CACHE_DIR") == "" {
		// A service (systemd, a launchd daemon) often runs without HOME.
		// On Linux restic then has no cache: every call re-reads the
		// repository's index from the bucket (minutes for a few hundred
		// thousand files, on pantry); on macOS it refuses to run at all
		// ("unable to locate cache directory: $HOME is not defined",
		// basement). Keep restic's cache in the state directory, where
		// XDG_CACHE_HOME put it on Linux before (<state>/cache/restic).
		os.Setenv("RESTIC_CACHE_DIR", filepath.Join(filepath.Dir(*db), "cache", "restic"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var listenErr atomic.Pointer[error]
	if *withParent {
		go func() {
			io.Copy(io.Discard, os.Stdin) // returns when the parent exits or closes the pipe
			stop()
		}()
	}
	// A remote: a daemon a controller pairs with (-listen), or any
	// daemon not on a Mac. The controller Mac's runs inside the app.
	remote := *listen != "" || runtime.GOOS != "darwin"
	core := daemon.NewCore(version, store, daemon.ResticEngines, nil)
	core.Name = *name
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			core.Build, _ = daemon.BuildID(exe)
			if remote {
				// The controller Mac's daemon ships inside the signed
				// app; remotes take updates from their controller
				// (self_update).
				core.UpdatePath, selfPath = exe, exe
				core.UpdateBundle = daemon.BundleOf(exe)
				core.Restart = func() { restart = true; stop() }
				if exe == installedExe() {
					core.Uninstall = func() { uninstall = true; stop() }
				}
			}
		}
	}
	// A copy of the database, backed up daily into the repository.
	core.StateCopy = filepath.Join(filepath.Dir(*db), "charm-bak.db.bak")
	// Snapshot listings kept between runs: a cache database of its own.
	if err := core.SetTreeCache(filepath.Join(filepath.Dir(*db), "cache", "trees.db")); err != nil {
		log.Printf("snapshot listings stay in memory only: %v", err)
	}
	if remote {
		// No Keychain (or, on a Mac remote, a root daemon with no login
		// Keychain): repository_set keeps secrets in owner-only files
		// beside the database.
		core.SecretDir = filepath.Dir(*db)
	}
	if exe, err := os.Executable(); err == nil {
		// In the app bundle: Contents/MacOS/charm-bak-daemon, and the
		// remotes' daemons in Contents/Resources (linux-<arch>/,
		// darwin-<arch>/charm-bak.app).
		core.SetRemoteDaemons(filepath.Join(filepath.Dir(exe), "..", "Resources"))
	}
	if err := core.StartRemotes(ctx, id); err != nil {
		fail(err)
	}
	if *listen != "" {
		srv, err := daemon.ListenRemote(core, *listen, id)
		if err != nil {
			fail(err)
		}
		go func() {
			// A listener that stops is a remote its controller can't
			// reach: shut down cleanly and exit non-zero, so the service
			// manager starts a fresh daemon.
			if err := srv.Serve(); err != nil {
				log.Printf("the listener for controllers stopped: %v", err)
				listenErr.Store(&err)
				stop()
			}
		}()
		defer srv.Close()
	}
	build := core.Build
	if len(build) > 12 {
		build = build[:12]
	}
	listening := ""
	if *listen != "" {
		listening = ", paired controllers on " + *listen
	}
	log.Printf("charm-bak-daemon %s (build %s, %s/%s) starting as %s on %s%s", version, build, runtime.GOOS, runtime.GOARCH, core.Name, *socket, listening)
	if err := daemon.Run(ctx, core, *socket); err != nil {
		log.Printf("stopping: %v", err)
		fail(err)
	}
	if err := listenErr.Load(); err != nil {
		fail(fmt.Errorf("the listener for controllers stopped: %w", *err))
	}
	if uninstall {
		log.Printf("stopped; uninstalling")
		removeInstall(filepath.Dir(*db))
		return false
	}
	if restart {
		log.Printf("stopped; restarting into the update")
	} else {
		log.Printf("stopped")
	}
	return restart
}

// What the pairing script installs (pairing.go): on Linux a systemd
// service; on a Mac a launchd daemon running the executable inside a
// small app bundle (Full Disk Access is granted to the bundle).
const (
	installedPath = "/usr/local/bin/charm-bak-daemon"
	installedUnit = "/etc/systemd/system/charm-bak.service"
	macInstallDir = "/Library/Application Support/charm-bak"
	macLabel      = daemon.MacLabel
	macPlist      = "/Library/LaunchDaemons/" + macLabel + ".plist"
)

func installedExe() string {
	if runtime.GOOS == "darwin" {
		return macInstallDir + "/charm-bak.app/Contents/MacOS/charm-bak-daemon"
	}
	return installedPath
}

// removeInstall undoes the pairing script: the service (so exiting 0 now
// isn't restarted), the executable, and the state directory with the
// database, secrets and identity key. The repository is left alone;
// uninstall deleted its snapshots if asked.
func removeInstall(stateDir string) {
	run := func(args ...string) {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			log.Printf("%v: %v %s", args, err, out)
		}
	}
	if runtime.GOOS == "darwin" {
		os.Remove(macPlist)
		os.RemoveAll(macInstallDir) // the bundle and state/
		log.Printf("uninstalled")
		// Last: launchd stops this process when its job goes.
		run("launchctl", "bootout", "system/"+macLabel)
		return
	}
	run("systemctl", "disable", "charm-bak.service")
	os.Remove(installedUnit)
	run("systemctl", "daemon-reload")
	os.Remove(installedPath)
	if filepath.Base(stateDir) == "charm-bak" { // never anything broader
		os.RemoveAll(stateDir)
	}
	log.Printf("uninstalled")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "charm-bak-daemon:", err)
	os.Exit(1)
}
