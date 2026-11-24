package client

import (
	"context"
	"os"
	"testing"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// TestLiveApp walks the real app's data: sources, snapshots, the tree
// root of the newest one, and a file in it. Opt in with CHARM_BAK_LIVE=1
// while the app is running with a repository.
func TestLiveApp(t *testing.T) {
	if os.Getenv("CHARM_BAK_LIVE") == "" {
		t.Skip("set CHARM_BAK_LIVE=1 to run against the installed app")
	}
	ctx := context.Background()
	c, err := Dial(ctx, protocol.SocketPath(), "live-test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("app %s", c.AppVersion())
	sources, err := c.Sources(ctx)
	if err != nil || len(sources) == 0 {
		t.Fatalf("sources: %v %v", sources, err)
	}
	s := sources[0]
	t.Logf("source %s %v health=%s", s.Name, s.Paths, s.Health)
	snaps, err := c.Snapshots(ctx, s.ID)
	if err != nil || len(snaps) == 0 {
		t.Fatalf("snapshots: %v %v", snaps, err)
	}
	root := backup.Root(snaps[0].Paths)
	entries, err := c.List(ctx, snaps[0].ID, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Logf("  %s %s %d", e.Type, e.Name, e.Size)
		if e.Type == protocol.TypeFile {
			d, err := c.Read(ctx, snaps[0].ID, backup.Join(root, e.Name), 100)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("    read %d of %d bytes: %q", len(d.Data), d.Size, d.Data)
		}
	}
}
