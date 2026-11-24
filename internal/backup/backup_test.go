package backup

import "testing"

func TestRoot(t *testing.T) {
	cases := []struct {
		paths []string
		want  string
	}{
		{nil, "/"},
		{[]string{"/Users/a/src/"}, "/Users/a/src"},
		{[]string{"/Users/a/src/x", "/Users/a/src/y"}, "/Users/a/src"},
		{[]string{"/Users/a/src", "/Users/a/src/y"}, "/Users/a/src"},
		{[]string{"/Users/a", "/Volumes/b"}, "/"},
	}
	for _, c := range cases {
		if got := Root(c.paths); got != c.want {
			t.Errorf("Root(%v) = %q, want %q", c.paths, got, c.want)
		}
	}
	if Join("/", "a") != "/a" || Join("/x", "a") != "/x/a" {
		t.Error("Join")
	}
}
