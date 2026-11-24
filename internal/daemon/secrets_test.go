package daemon

import (
	"os/exec"
	"strings"
	"testing"
)

// ShellEnv's output, evaluated by sh, sets the variables, and holds no
// secret itself (the key comes from a command run at eval time).
func TestShellEnvEvaluates(t *testing.T) {
	out, err := ShellEnv(RepositoryConfig{
		Location:        "s3:s3.amazonaws.com/b/it's",
		PasswordCommand: "security find-generic-password -s x -a y -w",
		Environment:     map[string]string{"AWS_DEFAULT_REGION": "us-east-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := exec.Command("sh", "-c", out+`printf '%s|%s|%s' "$RESTIC_REPOSITORY" "$RESTIC_PASSWORD_COMMAND" "$AWS_DEFAULT_REGION"`).Output()
	if err != nil {
		t.Fatalf("sh: %v\n%s", err, out)
	}
	want := "s3:s3.amazonaws.com/b/it's|security find-generic-password -s x -a y -w|us-east-1"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// The AWS line, with the Keychain read swapped for a stand-in.
	out, _ = ShellEnv(RepositoryConfig{Location: "x", AWSKeychain: "svc/acct"})
	if !strings.Contains(out, "security find-generic-password -s 'svc' -a 'acct' -w") {
		t.Fatalf("no Keychain read in %q", out)
	}
	stub := strings.Replace(out, "security find-generic-password -s 'svc' -a 'acct' -w", "echo AKIDEXAMPLE:se:cret", 1)
	got, err = exec.Command("sh", "-c", stub+`printf '%s|%s' "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY"`).Output()
	if err != nil || string(got) != "AKIDEXAMPLE|se:cret" {
		t.Errorf("aws: %q %v\n%s", got, err, stub)
	}
}
