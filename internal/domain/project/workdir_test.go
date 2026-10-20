package project

import "testing"

func TestParseWorkingDirectory(t *testing.T) {
	got, err := ParseWorkingDirectory("")
	if err != nil || got != "" {
		t.Fatalf("empty: %q %v", got, err)
	}
	got, err = ParseWorkingDirectory("terraform")
	if err != nil || got != "terraform" {
		t.Fatalf("relative: %q %v", got, err)
	}
	got, err = ParseWorkingDirectory("modules/net")
	if err != nil || got != "modules/net" {
		t.Fatalf("nested: %q %v", got, err)
	}
	for _, bad := range []string{"/tmp/x", "../x", "foo/../../etc", `C:\windows`} {
		if _, err := ParseWorkingDirectory(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
