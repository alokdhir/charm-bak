package daemon

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Secrets fetches a repository's secret environment for one engine run.
// The config only names where secrets live; they're read per run and
// passed to that process alone.
//
// On macOS that's the login Keychain, read with the `security` tool: the
// items were created with `security`, so they trust it and reading is
// silent while the user is logged in (reading them through the Keychain
// API would prompt). Elsewhere (a Linux remote) it's owner-only files in
// the state directory, written by repository_set.
func Secrets(r RepositoryConfig) (map[string]string, error) {
	env := map[string]string{}
	if service, account, ok := strings.Cut(r.EnvKeychain, "/"); ok {
		v, err := KeychainRead(service, account)
		if err != nil {
			return nil, fmt.Errorf("can't read the credentials from the Keychain (%s): %w", r.EnvKeychain, err)
		}
		if env, err = decodeEnv(v); err != nil {
			return nil, fmt.Errorf("the Keychain item %s: %w", r.EnvKeychain, err)
		}
	} else if r.EnvFile != "" {
		b, err := os.ReadFile(r.EnvFile)
		if err != nil {
			return nil, fmt.Errorf("can't read the credentials: %w", err)
		}
		if env, err = decodeEnv(string(b)); err != nil {
			return nil, fmt.Errorf("%s: %w", r.EnvFile, err)
		}
	}
	var pair, from string
	switch {
	case r.AWSKeychain != "":
		service, account, ok := strings.Cut(r.AWSKeychain, "/")
		if !ok {
			return nil, fmt.Errorf("aws_keychain %q isn't service/account", r.AWSKeychain)
		}
		v, err := KeychainRead(service, account)
		if err != nil {
			return nil, fmt.Errorf("can't read the AWS key from the Keychain (%s): %w", r.AWSKeychain, err)
		}
		pair, from = v, "the Keychain item "+r.AWSKeychain
	case r.AWSFile != "":
		b, err := os.ReadFile(r.AWSFile)
		if err != nil {
			return nil, fmt.Errorf("can't read the AWS key: %w", err)
		}
		pair, from = strings.TrimSpace(string(b)), r.AWSFile
	default:
		return env, nil
	}
	id, secret, ok := strings.Cut(pair, ":")
	if !ok {
		return nil, fmt.Errorf("%s isn't KEYID:SECRET", from)
	}
	env["AWS_ACCESS_KEY_ID"] = id
	env["AWS_SECRET_ACCESS_KEY"] = secret
	return env, nil
}

// KeychainCommand is the restic --password-command that reads a Keychain
// item silently.
func KeychainCommand(service, account string) string {
	return fmt.Sprintf("security find-generic-password -s %s -a %s -w", service, account)
}

// KeychainRead reads a generic password with `security` (macOS).
func KeychainRead(service, account string) (string, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// keychainDelete removes a generic password; one that isn't there is
// fine.
func keychainDelete(service, account string) {
	_ = exec.Command("security", "delete-generic-password", "-s", service, "-a", account).Run()
}

// keychainAdd stores a generic password that trusts `security`, so later
// reads are silent. The value goes to `security -i` on stdin, never on a
// command line where ps could show it.
func keychainAdd(service, account, label, value string) error {
	cmd := exec.Command("security", "-i")
	line, err := keychainAddLine(service, account, label, value)
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(line)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("can't add Keychain item %s/%s: %v %s", service, account, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// keychainAddLine is the `security -i` command that stores value. A
// value with a quote or backslash (an existing repository's password can
// hold anything) goes as hex (-X), which needs no quoting; a line break
// or control character can't be stored: reads come back through
// `security … -w`, which prints such a value as hex. (Tested on macOS
// 27.2, 2026-11-24: a value with a quote, a backslash and a space stored
// with -X reads back unchanged through -w.)
func keychainAddLine(service, account, label, value string) (string, error) {
	if strings.ContainsAny(service+account+label, "\"\\\n") {
		return "", fmt.Errorf("unexpected characters for a Keychain item")
	}
	if value == "" || strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("the Keychain can't hold a value that's empty or has line breaks or control characters")
	}
	how := "-w \"" + value + "\""
	if strings.ContainsAny(value, "\"\\") {
		how = "-X " + hex.EncodeToString([]byte(value))
	}
	return fmt.Sprintf("add-generic-password -U -s \"%s\" -a \"%s\" -l \"%s\" -T /usr/bin/security %s\n", service, account, label, how), nil
}

// newPassword is a fresh repository password: 32 random bytes, base64url.
func newPassword() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Secret file names in the state directory.
const (
	passwordFileName = "repo-password"
	awsFileName      = "aws"
	envFileName      = "credentials"
)

// writeSecret writes value to dir/name, owner-only, replacing it
// atomically. dir is made owner-only too.
func writeSecret(dir, name, value string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return "", err
	}
	if _, err := f.WriteString(value + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	return path, os.Rename(f.Name(), path)
}

// ShellEnv is the repository as shell exports, for running restic by
// hand: eval "$(charm-bak-daemon -env)". It holds no secrets: the AWS
// key and the password are read (from the Keychain or the owner-only
// files) by commands that run when the exports are evaluated.
func ShellEnv(r RepositoryConfig) (string, error) {
	var b strings.Builder
	export := func(name, value string) { fmt.Fprintf(&b, "export %s=%s\n", name, shellQuote(value)) }
	export("RESTIC_REPOSITORY", r.Location)
	if r.PasswordCommand != "" {
		export("RESTIC_PASSWORD_COMMAND", r.PasswordCommand)
	}
	if r.PasswordFile != "" {
		export("RESTIC_PASSWORD_FILE", r.PasswordFile)
	}
	keys := make([]string, 0, len(r.Environment))
	for k := range r.Environment {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		export(k, r.Environment[k])
	}
	var read string
	switch {
	case r.AWSKeychain != "":
		service, account, ok := strings.Cut(r.AWSKeychain, "/")
		if !ok {
			return "", fmt.Errorf("aws_keychain %q isn't service/account", r.AWSKeychain)
		}
		read = "security find-generic-password -s " + shellQuote(service) + " -a " + shellQuote(account) + " -w"
	case r.AWSFile != "":
		read = "cat " + shellQuote(r.AWSFile)
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return "", err
	}
	if service, account, ok := strings.Cut(r.EnvKeychain, "/"); ok {
		// The credentials go from the Keychain straight into eval.
		fmt.Fprintf(&b, "eval \"$(security find-generic-password -s %s -a %s -w | %s -exports)\"\n", shellQuote(service), shellQuote(account), shellQuote(exe))
	} else if r.EnvFile != "" {
		fmt.Fprintf(&b, "eval \"$(%s -exports < %s)\"\n", shellQuote(exe), shellQuote(r.EnvFile))
	}
	if read != "" {
		// One read, split into the two variables.
		fmt.Fprintf(&b, "_cbk=\"$(%s)\" && export AWS_ACCESS_KEY_ID=\"${_cbk%%%%:*}\" AWS_SECRET_ACCESS_KEY=\"${_cbk#*:}\"; unset _cbk\n", read)
	}
	return b.String(), nil
}

// shellQuote single-quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// encodeEnv is how a credential set is stored: JSON, base64url-encoded
// so a Keychain item written through `security -i` needs no quoting.
func encodeEnv(env map[string]string) string {
	b, _ := json.Marshal(env)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeEnv reads a stored credential set: encodeEnv's form, or plain
// JSON (a file edited by hand).
func decodeEnv(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	raw := []byte(s)
	if !strings.HasPrefix(s, "{") {
		var err error
		if raw, err = base64.RawURLEncoding.DecodeString(s); err != nil {
			return nil, fmt.Errorf("credentials aren't readable")
		}
	}
	env := map[string]string{}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("credentials aren't readable")
	}
	return env, nil
}

// Exports turns a stored credential set into shell exports, for -env's
// eval line; it reads the set on stdin, so it's never on a command line.
func Exports(stored string) (string, error) {
	env, err := decodeEnv(stored)
	if err != nil {
		return "", err
	}
	keys := slices.Sorted(maps.Keys(env))
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(env[k]))
	}
	return b.String(), nil
}
