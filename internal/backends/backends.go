// Package backends describes where a restic repository can live: each
// backend's fields, how they make a location, and which environment
// variables carry its credentials. The setup flow asks for the fields;
// the daemon stores the credentials and passes them to restic.
//
// Location syntax and variable names are from restic's docs ("Preparing a
// new repository", "Scripting: environment variables"), stable docs read
// 2026-11-22 against restic 0.19.1.
package backends

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Field is one thing the setup flow asks for.
type Field struct {
	Key   string // "bucket", "path", …; or, for a credential, its variable
	Label string
	// Env is set for credentials: the environment variable restic reads
	// it from. Credentials are stored as secrets, never in the config.
	Env    string
	Secret bool // masked while typed
	// Optional fields may stay empty.
	Optional    bool
	Placeholder string
	// Default prefills the field; "{host}" is the machine's name.
	Default string
	Help    string
}

// Backend is one kind of place to keep a repository.
type Backend struct {
	ID, Label string
	Blurb     string // one line under the label in the picker
	Fields    []Field
	// Note is shown under the fields (what this backend relies on).
	Note string
	// location makes the restic location from the non-credential fields.
	location func(v map[string]string) (string, error)
	// extraEnv is non-secret environment derived from the fields, e.g.
	// the region an S3-compatible endpoint signs with.
	extraEnv func(v map[string]string) map[string]string
}

// All backends, in the order the picker lists them.
var All = []Backend{
	{
		ID: "s3", Label: "Amazon S3", Blurb: "a bucket in your AWS account",
		Fields: []Field{
			{Key: "bucket", Label: "Bucket", Placeholder: "my-backups"},
			{Key: "folder", Label: "Folder", Default: "{host}", Help: "one repository per machine, side by side in the bucket"},
			{Key: "region", Label: "Region", Optional: true, Placeholder: "us-east-1", Help: "leave empty to let restic find it"},
			{Key: "AWS_ACCESS_KEY_ID", Env: "AWS_ACCESS_KEY_ID", Label: "Access key", Placeholder: "AKIA…"},
			{Key: "AWS_SECRET_ACCESS_KEY", Env: "AWS_SECRET_ACCESS_KEY", Label: "Secret", Secret: true},
		},
		Note: "Use a key that can only reach this bucket. Storage class Intelligent-Tiering suits backups; never Glacier.",
		location: func(v map[string]string) (string, error) {
			host := "s3.amazonaws.com"
			if r := v["region"]; r != "" {
				host = "s3." + r + ".amazonaws.com"
			}
			return "s3:" + host + "/" + join(v["bucket"], v["folder"]), nil
		},
	},
	{
		ID: "b2", Label: "Backblaze B2", Blurb: "cheap object storage, through its S3 API",
		Fields: []Field{
			{Key: "endpoint", Label: "Endpoint", Placeholder: "s3.us-west-004.backblazeb2.com", Help: "shown as Endpoint on the bucket's page"},
			{Key: "bucket", Label: "Bucket", Placeholder: "my-backups"},
			{Key: "folder", Label: "Folder", Default: "{host}"},
			{Key: "AWS_ACCESS_KEY_ID", Env: "AWS_ACCESS_KEY_ID", Label: "Key ID", Placeholder: "004…"},
			{Key: "AWS_SECRET_ACCESS_KEY", Env: "AWS_SECRET_ACCESS_KEY", Label: "App key", Secret: true},
		},
		// restic's docs recommend B2's S3 API over its native b2: backend.
		Note: "An application key limited to this bucket is enough.",
		location: func(v map[string]string) (string, error) {
			ep := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(v["endpoint"], "https://"), "http://"), "/")
			if !strings.Contains(ep, ".") {
				return "", fmt.Errorf("the endpoint looks like s3.us-west-004.backblazeb2.com")
			}
			return "s3:https://" + ep + "/" + join(v["bucket"], v["folder"]), nil
		},
		extraEnv: func(v map[string]string) map[string]string {
			// The S3 API signs with the region in the endpoint's name;
			// restic otherwise assumes us-east-1.
			if m := b2Region.FindStringSubmatch(v["endpoint"]); m != nil {
				return map[string]string{"AWS_DEFAULT_REGION": m[1]}
			}
			return nil
		},
	},
	{
		ID: "s3compat", Label: "Other S3-compatible", Blurb: "Wasabi, Cloudflare R2, MinIO, …",
		Fields: []Field{
			{Key: "endpoint", Label: "Endpoint", Placeholder: "https://s3.wasabisys.com"},
			{Key: "bucket", Label: "Bucket", Placeholder: "my-backups"},
			{Key: "folder", Label: "Folder", Default: "{host}"},
			{Key: "region", Label: "Region", Optional: true, Placeholder: "us-east-1", Help: "if the service asks for one"},
			{Key: "AWS_ACCESS_KEY_ID", Env: "AWS_ACCESS_KEY_ID", Label: "Access key"},
			{Key: "AWS_SECRET_ACCESS_KEY", Env: "AWS_SECRET_ACCESS_KEY", Label: "Secret", Secret: true},
		},
		location: func(v map[string]string) (string, error) {
			ep := strings.TrimSuffix(v["endpoint"], "/")
			if !strings.Contains(ep, "://") {
				ep = "https://" + ep
			}
			if _, err := url.Parse(ep); err != nil {
				return "", fmt.Errorf("the endpoint isn't a URL")
			}
			return "s3:" + ep + "/" + join(v["bucket"], v["folder"]), nil
		},
		extraEnv: func(v map[string]string) map[string]string {
			if r := v["region"]; r != "" {
				return map[string]string{"AWS_DEFAULT_REGION": r}
			}
			return nil
		},
	},
	{
		ID: "local", Label: "Folder or drive", Blurb: "an external disk, a NAS share, any folder",
		Fields: []Field{
			{Key: "path", Label: "Folder", Placeholder: "/Volumes/Backup/charm-bak", Help: "made if it isn't there; its drive must be"},
		},
		Note: "Its drive has to be connected when backups run.",
		location: func(v map[string]string) (string, error) {
			p := v["path"]
			if !strings.HasPrefix(p, "/") {
				return "", fmt.Errorf("the folder needs a full path, starting with /")
			}
			return strings.TrimSuffix(p, "/"), nil
		},
	},
	{
		ID: "rest", Label: "REST server", Blurb: "restic's own rest-server, on your network",
		Fields: []Field{
			{Key: "url", Label: "URL", Placeholder: "https://nas.local:8000/{host}/"},
			{Key: "RESTIC_REST_USERNAME", Env: "RESTIC_REST_USERNAME", Label: "Username", Optional: true},
			{Key: "RESTIC_REST_PASSWORD", Env: "RESTIC_REST_PASSWORD", Label: "Password", Secret: true, Optional: true},
		},
		location: func(v map[string]string) (string, error) {
			u := v["url"]
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				return "", fmt.Errorf("the URL starts with http:// or https://")
			}
			return "rest:" + u, nil
		},
	},
	{
		ID: "sftp", Label: "SFTP", Blurb: "any machine you can ssh to",
		Fields: []Field{
			{Key: "server", Label: "Server", Placeholder: "you@nas.local"},
			{Key: "path", Label: "Folder", Placeholder: "/srv/backups/{host}", Help: "a full path, or relative to your home there"},
		},
		Note: "Logs in with this machine's SSH keys; it must connect without a password prompt.",
		location: func(v map[string]string) (string, error) {
			return "sftp:" + v["server"] + ":" + v["path"], nil
		},
	},
	{
		ID: "azure", Label: "Azure Blob Storage", Blurb: "a container in a storage account",
		Fields: []Field{
			{Key: "AZURE_ACCOUNT_NAME", Env: "AZURE_ACCOUNT_NAME", Label: "Account"},
			{Key: "AZURE_ACCOUNT_KEY", Env: "AZURE_ACCOUNT_KEY", Label: "Account key", Secret: true},
			{Key: "container", Label: "Container", Placeholder: "backups"},
			{Key: "folder", Label: "Folder", Default: "{host}"},
		},
		location: func(v map[string]string) (string, error) {
			return "azure:" + v["container"] + ":/" + v["folder"], nil
		},
	},
	{
		ID: "gs", Label: "Google Cloud Storage", Blurb: "a bucket in a Google Cloud project",
		Fields: []Field{
			{Key: "GOOGLE_PROJECT_ID", Env: "GOOGLE_PROJECT_ID", Label: "Project ID"},
			{Key: "GOOGLE_APPLICATION_CREDENTIALS", Env: "GOOGLE_APPLICATION_CREDENTIALS", Label: "Key file", Placeholder: "/path/to/service-account.json", Help: "a service account's JSON key, on that machine"},
			{Key: "bucket", Label: "Bucket"},
			{Key: "folder", Label: "Folder", Default: "{host}"},
		},
		location: func(v map[string]string) (string, error) {
			return "gs:" + v["bucket"] + ":/" + v["folder"], nil
		},
	},
	{
		ID: "rclone", Label: "rclone", Blurb: "anything rclone reaches: Drive, Dropbox, OneDrive, …",
		Fields: []Field{
			{Key: "remote", Label: "Remote", Placeholder: "gdrive", Help: "a remote set up with rclone config, on that machine"},
			{Key: "path", Label: "Folder", Default: "charm-bak/{host}"},
		},
		Note: "Needs rclone installed and configured on the machine being backed up.",
		location: func(v map[string]string) (string, error) {
			return "rclone:" + v["remote"] + ":" + v["path"], nil
		},
	},
}

var b2Region = regexp.MustCompile(`^(?:https?://)?s3\.([a-z0-9-]+)\.backblazeb2\.com`)

// ByID finds a backend.
func ByID(id string) (Backend, bool) {
	for _, b := range All {
		if b.ID == id {
			return b, true
		}
	}
	return Backend{}, false
}

// Detect says which backend a location belongs to, for showing an
// existing repository; "" if none matches.
func Detect(location string) string {
	switch {
	case strings.HasPrefix(location, "s3:s3.") && strings.Contains(location, "amazonaws.com/"):
		return "s3"
	case strings.HasPrefix(location, "s3:") && strings.Contains(location, ".backblazeb2.com/"):
		return "b2"
	case strings.HasPrefix(location, "s3:"):
		return "s3compat"
	case strings.HasPrefix(location, "rest:"):
		return "rest"
	case strings.HasPrefix(location, "sftp:"):
		return "sftp"
	case strings.HasPrefix(location, "azure:"):
		return "azure"
	case strings.HasPrefix(location, "gs:"):
		return "gs"
	case strings.HasPrefix(location, "rclone:"):
		return "rclone"
	case strings.HasPrefix(location, "/"):
		return "local"
	}
	return ""
}

// Default is a field's starting value for a machine.
func (f Field) DefaultFor(host string) string {
	return strings.ReplaceAll(f.Default, "{host}", host)
}

// Build checks the values and returns the location and the environment
// restic needs (credentials and anything derived, like a region).
// Values are trimmed; required ones must be present.
func (b Backend) Build(values map[string]string) (location string, env map[string]string, err error) {
	v := map[string]string{}
	for _, f := range b.Fields {
		s := strings.TrimSpace(values[f.Key])
		if s == "" && !f.Optional {
			return "", nil, fmt.Errorf("%s is empty", f.Label)
		}
		if strings.ContainsAny(s, "\n\r") {
			return "", nil, fmt.Errorf("%s has a line break", f.Label)
		}
		v[f.Key] = s
	}
	if location, err = b.location(v); err != nil {
		return "", nil, err
	}
	env = map[string]string{}
	for _, f := range b.Fields {
		if f.Env != "" && v[f.Key] != "" {
			env[f.Env] = v[f.Key]
		}
	}
	if b.extraEnv != nil {
		for k, val := range b.extraEnv(v) {
			env[k] = val
		}
	}
	return location, env, nil
}

// EnvPrefixes are the variable families backends read. A daemon drops
// them from its own environment before running restic, so only the
// repository's credentials reach it.
var EnvPrefixes = []string{"RESTIC_", "AWS_", "B2_", "AZURE_", "GOOGLE_", "OS_", "ST_", "RCLONE_"}

// join makes "bucket/folder", skipping an empty folder.
func join(bucket, folder string) string {
	folder = strings.Trim(folder, "/")
	if folder == "" {
		return strings.Trim(bucket, "/")
	}
	return strings.Trim(bucket, "/") + "/" + folder
}

// Parse reads a location back into its backend's non-credential fields,
// so a new machine's setup can start from another machine's repository.
// ok is false for a location Detect doesn't know.
func Parse(location string) (id string, values map[string]string, ok bool) {
	id = Detect(location)
	v := map[string]string{}
	// bucketFolder splits "bucket/folder/…" at the first slash.
	bucketFolder := func(s string) {
		b, f, _ := strings.Cut(strings.Trim(s, "/"), "/")
		v["bucket"], v["folder"] = b, f
	}
	switch id {
	case "s3":
		host, rest, _ := strings.Cut(strings.TrimPrefix(location, "s3:"), "/")
		if host != "s3.amazonaws.com" {
			v["region"] = strings.TrimSuffix(strings.TrimPrefix(host, "s3."), ".amazonaws.com")
		}
		bucketFolder(rest)
	case "b2", "s3compat":
		u := strings.TrimPrefix(location, "s3:")
		scheme := ""
		if i := strings.Index(u, "://"); i >= 0 {
			scheme, u = u[:i+3], u[i+3:]
		}
		host, rest, _ := strings.Cut(u, "/")
		if id == "b2" {
			v["endpoint"] = host
		} else {
			if scheme == "https://" {
				scheme = ""
			}
			v["endpoint"] = scheme + host
		}
		bucketFolder(rest)
	case "local":
		v["path"] = location
	case "rest":
		v["url"] = strings.TrimPrefix(location, "rest:")
	case "sftp":
		server, path, _ := strings.Cut(strings.TrimPrefix(location, "sftp:"), ":")
		v["server"], v["path"] = server, path
	case "azure", "gs":
		bucket, folder, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(location, "azure:"), "gs:"), ":")
		key := "bucket"
		if id == "azure" {
			key = "container"
		}
		v[key], v["folder"] = bucket, strings.Trim(folder, "/")
	case "rclone":
		remote, path, _ := strings.Cut(strings.TrimPrefix(location, "rclone:"), ":")
		v["remote"], v["path"] = remote, path
	default:
		return "", nil, false
	}
	return id, v, true
}

// For is where another machine's repository would go, beside this one's:
// the same backend and values, with the field that names the machine
// (one whose default or example has "{host}": its last element when
// that's from's name, else its default) switched to host. Credentials aren't copied; the caller has none.
func (b Backend) For(values map[string]string, from, host string) map[string]string {
	out := map[string]string{}
	for _, f := range b.Fields {
		if f.Env != "" {
			continue
		}
		s := values[f.Key]
		// Only a field that names the machine (its default or its
		// example has "{host}") is switched: a bucket that happens to be
		// called what this machine is stays the same bucket.
		names := strings.Contains(f.Default, "{host}") || strings.Contains(f.Placeholder, "{host}")
		if dir, last := splitLast(s); names && last == from && from != "" {
			s = dir + host
		} else if strings.Contains(f.Default, "{host}") {
			s = f.DefaultFor(host)
		}
		out[f.Key] = s
	}
	return out
}

func splitLast(s string) (dir, last string) {
	s = strings.TrimSuffix(s, "/")
	i := strings.LastIndex(s, "/")
	return s[:i+1], s[i+1:]
}
