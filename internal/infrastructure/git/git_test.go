package git

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneArgsKeepsURLClean(t *testing.T) {
	url := "https://github.com/owner/repo.git"
	token := "s3cret-token"
	args := cloneArgs(url, token, "deadbeef")
	wantHeader := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))

	var foundURL, foundAuth bool
	for _, a := range args {
		if strings.Contains(a, "x-access-token") || strings.Contains(a, token) {
			t.Fatalf("args contain cleartext token or x-access-token: %q", args)
		}
		if a == url {
			foundURL = true
		}
		if strings.Contains(a, wantHeader) {
			foundAuth = true
		}
	}
	if !foundURL {
		t.Fatalf("URL argument want %s in %q", url, args)
	}
	if !foundAuth {
		t.Fatalf("missing extraheader %q in %q", wantHeader, args)
	}
}

func TestCloneArgsOmitsHeaderWhenTokenEmpty(t *testing.T) {
	url := "https://github.com/owner/repo.git"
	args := cloneArgs(url, "", "deadbeef")
	for _, a := range args {
		if a == "-c" || strings.Contains(strings.ToLower(a), "authorization") {
			t.Fatalf("unexpected auth argument %q in %q", a, args)
		}
	}
	foundURL := false
	for _, a := range args {
		if a == url {
			foundURL = true
		}
	}
	if !foundURL {
		t.Fatalf("URL argument want %s in %q", url, args)
	}
}

func TestCheckoutBranchTipAndClone(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	runGit(t, work, "init", "-b", "main")
	runGit(t, work, "config", "user.email", "infra-guy@example.com")
	runGit(t, work, "config", "user.name", "infra-guy")
	if err := os.WriteFile(filepath.Join(work, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "hello.txt")
	runGit(t, work, "commit", "-m", "add hello")
	sha := strings.TrimSpace(runGit(t, work, "rev-parse", "HEAD"))

	bare := t.TempDir()
	runGit(t, bare, "init", "--bare", "-b", "main")
	runGit(t, work, "remote", "add", "origin", bare)
	runGit(t, work, "push", "origin", "HEAD:main")

	var c Checkout
	tip, err := c.BranchTip(ctx, bare, "", "main")
	if err != nil {
		t.Fatal(err)
	}
	if tip != sha {
		t.Fatalf("BranchTip = %s, want %s", tip, sha)
	}

	dest := t.TempDir()
	if err := c.Clone(ctx, dest, bare, "", sha); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("cloned file = %q", got)
	}
	head := strings.TrimSpace(runGit(t, dest, "rev-parse", "HEAD"))
	if head != sha {
		t.Fatalf("cloned HEAD = %s, want %s", head, sha)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
