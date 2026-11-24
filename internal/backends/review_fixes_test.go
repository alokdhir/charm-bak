package backends

import "testing"

// For switches the field that names the machine, never one that only
// happens to equal its name (a bucket called what this Mac is).
func TestForLeavesBucketNamedLikeHost(t *testing.T) {
	var s3 Backend
	for _, b := range All {
		if b.ID == "s3" {
			s3 = b
		}
	}
	got := s3.For(map[string]string{"bucket": "studio", "folder": "restic"}, "studio", "pantry")
	if got["bucket"] != "studio" {
		t.Errorf("bucket became %q", got["bucket"])
	}
	if got["folder"] != "pantry" {
		t.Errorf("folder %q, want the machine's default (pantry)", got["folder"])
	}
	// The usual layout still moves the folder.
	got = s3.For(map[string]string{"bucket": "example-backups", "folder": "studio"}, "studio", "pantry")
	if got["bucket"] != "example-backups" || got["folder"] != "pantry" {
		t.Errorf("got %v", got)
	}
	got = s3.For(map[string]string{"bucket": "b", "folder": "backups/studio"}, "studio", "pantry")
	if got["folder"] != "backups/pantry" {
		t.Errorf("nested folder %q", got["folder"])
	}
}
