package backends

import (
	"maps"
	"testing"
)

func TestBuild(t *testing.T) {
	cases := []struct {
		id      string
		values  map[string]string
		loc     string
		env     map[string]string
		wantErr bool
	}{
		{"s3", map[string]string{"bucket": "example-backups", "folder": "studio", "AWS_ACCESS_KEY_ID": "AKIA1", "AWS_SECRET_ACCESS_KEY": "s"},
			"s3:s3.amazonaws.com/example-backups/studio", map[string]string{"AWS_ACCESS_KEY_ID": "AKIA1", "AWS_SECRET_ACCESS_KEY": "s"}, false},
		{"s3", map[string]string{"bucket": "b", "folder": "/m/", "region": "eu-west-1", "AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s"},
			"s3:s3.eu-west-1.amazonaws.com/b/m", nil, false},
		{"s3", map[string]string{"bucket": "b", "folder": "m", "AWS_ACCESS_KEY_ID": "k"}, "", nil, true},
		{"b2", map[string]string{"endpoint": "https://s3.us-west-004.backblazeb2.com/", "bucket": "b", "folder": "m", "AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s"},
			"s3:https://s3.us-west-004.backblazeb2.com/b/m", map[string]string{"AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s", "AWS_DEFAULT_REGION": "us-west-004"}, false},
		{"s3compat", map[string]string{"endpoint": "s3.wasabisys.com", "bucket": "b", "folder": "m", "AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s"},
			"s3:https://s3.wasabisys.com/b/m", nil, false},
		{"local", map[string]string{"path": "/Volumes/Backup/charm-bak/"}, "/Volumes/Backup/charm-bak", map[string]string{}, false},
		{"local", map[string]string{"path": "Backup"}, "", nil, true},
		{"rest", map[string]string{"url": "https://nas.local:8000/studio/"}, "rest:https://nas.local:8000/studio/", map[string]string{}, false},
		{"rest", map[string]string{"url": "nas.local:8000"}, "", nil, true},
		{"sftp", map[string]string{"server": "me@nas", "path": "/srv/b"}, "sftp:me@nas:/srv/b", map[string]string{}, false},
		{"azure", map[string]string{"AZURE_ACCOUNT_NAME": "a", "AZURE_ACCOUNT_KEY": "k", "container": "c", "folder": "m"}, "azure:c:/m", nil, false},
		{"gs", map[string]string{"GOOGLE_PROJECT_ID": "p", "GOOGLE_APPLICATION_CREDENTIALS": "/k.json", "bucket": "b", "folder": "m"}, "gs:b:/m", nil, false},
		{"rclone", map[string]string{"remote": "gdrive", "path": "charm-bak/m"}, "rclone:gdrive:charm-bak/m", map[string]string{}, false},
	}
	for _, c := range cases {
		b, ok := ByID(c.id)
		if !ok {
			t.Fatalf("no backend %s", c.id)
		}
		loc, env, err := b.Build(c.values)
		if (err != nil) != c.wantErr {
			t.Errorf("%s %v: err %v", c.id, c.values, err)
			continue
		}
		if c.wantErr {
			continue
		}
		if loc != c.loc {
			t.Errorf("%s: location %q, want %q", c.id, loc, c.loc)
		}
		if c.env != nil && !maps.Equal(env, c.env) {
			t.Errorf("%s: env %v, want %v", c.id, env, c.env)
		}
		if got := Detect(loc); got != c.id {
			t.Errorf("Detect(%q) = %q, want %q", loc, got, c.id)
		}
	}
}

func TestDefaults(t *testing.T) {
	b, _ := ByID("s3")
	for _, f := range b.Fields {
		if f.Key == "folder" && f.DefaultFor("pantry") != "pantry" {
			t.Errorf("folder default %q", f.DefaultFor("pantry"))
		}
	}
	for _, b := range All {
		for _, f := range b.Fields {
			if f.Secret && f.Env == "" {
				t.Errorf("%s: secret field %s isn't a credential, so it would be stored in the config", b.ID, f.Key)
			}
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct{ loc, id string }{
		{"s3:s3.amazonaws.com/example-backups/studio", "s3"},
		{"s3:s3.eu-west-1.amazonaws.com/b/m/n", "s3"},
		{"s3:https://s3.us-west-004.backblazeb2.com/b/m", "b2"},
		{"s3:https://s3.wasabisys.com/b/m", "s3compat"},
		{"s3:http://minio.local:9000/b/m", "s3compat"},
		{"/Volumes/Backup/charm-bak", "local"},
		{"rest:https://nas.local:8000/studio/", "rest"},
		{"sftp:me@nas:/srv/b", "sftp"},
		{"azure:c:/m", "azure"},
		{"gs:b:/m", "gs"},
		{"rclone:gdrive:charm-bak/m", "rclone"},
	}
	for _, c := range cases {
		id, v, ok := Parse(c.loc)
		if !ok || id != c.id {
			t.Errorf("Parse(%q) = %q %v", c.loc, id, ok)
			continue
		}
		b, _ := ByID(id)
		// Credentials make Build happy; they don't change the location.
		for _, f := range b.Fields {
			if f.Env != "" {
				v[f.Key] = "x"
			}
		}
		loc, _, err := b.Build(v)
		if err != nil || loc != c.loc {
			t.Errorf("Build(Parse(%q)) = %q, %v", c.loc, loc, err)
		}
	}
}

func TestFor(t *testing.T) {
	_, v, _ := Parse("s3:s3.amazonaws.com/example-backups/studio")
	b, _ := ByID("s3")
	got := b.For(v, "studio", "tower")
	if got["bucket"] != "example-backups" || got["folder"] != "tower" || got["AWS_ACCESS_KEY_ID"] != "" {
		t.Errorf("For = %v", got)
	}
	_, v, _ = Parse("rclone:gdrive:backups/studio")
	b, _ = ByID("rclone")
	if got := b.For(v, "studio", "tower"); got["path"] != "backups/tower" || got["remote"] != "gdrive" {
		t.Errorf("rclone For = %v", got)
	}
}
