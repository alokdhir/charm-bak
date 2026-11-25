package daemon

import "testing"

// A remote's word is taken for locations that carry no command, never
// for ones that would run something here.
func TestSafeRemoteLocation(t *testing.T) {
	for loc, want := range map[string]bool{
		"s3:s3.amazonaws.com/example-backups/pantry": true,
		"b2:bucket:pantry":                           true,
		"rest:https://backup.lan:8000/pantry":        true,
		"/Volumes/Backup/pantry":                     true,
		"sftp:alok@nas.lan:/backups/pantry":          true,
		"sftp://alok@nas.lan:2222//backups/pantry":   true,
		"rclone:onedrive:backups/pantry":             true,
		"sftp:-oProxyCommand=touch /tmp/x:/r":        false,
		"sftp:alok@-oProxyCommand=sh:/r":             false,
		"sftp:-l:/r":                                 false,
		"rclone::sftp,ssh='sh -c x':/r":              false,
		"rclone:remote,ssh=x:/r":                     false,
		"s3:-x":                                      false,
		"rest:file:///etc":                           false,
		"relative/path":                              false,
		"local:/tmp/x":                               false,
		"s3:host/bucket with space":                  false,
	} {
		if got := safeRemoteLocation(loc); got != want {
			t.Errorf("safeRemoteLocation(%q) = %v, want %v", loc, got, want)
		}
	}
}
