package version

import "testing"

func TestFormatUsesTagOrShortCommit(t *testing.T) {
	cases := []struct {
		name     string
		stamped  string
		tag      string
		revision string
		dirty    bool
		want     string
	}{
		{name: "stamped tag", stamped: "v0.1.43", tag: "v0.1.42", revision: "abcdef1234567890", want: "v0.1.43"},
		{name: "clean tag", tag: "v0.1.43", revision: "abcdef1234567890", want: "v0.1.43"},
		{name: "dirty tag falls back to commit", tag: "v0.1.43", revision: "abcdef1234567890", dirty: true, want: "abcdef1"},
		{name: "untagged commit", revision: "9db2e00abc", want: "9db2e00"},
		{name: "short revision stays", revision: "abc", want: "abc"},
		{name: "missing metadata", want: "dev"},
		{name: "dev stamp with commit", stamped: "dev", revision: "305dd3f000", want: "305dd3f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := format(tc.stamped, tc.tag, tc.revision, tc.dirty)
			if got != tc.want {
				t.Fatalf("format() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCurrentIsVisible(t *testing.T) {
	if Current() == "" {
		t.Fatal("empty version")
	}
}
