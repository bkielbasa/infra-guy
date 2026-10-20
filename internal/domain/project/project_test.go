package project

import (
	"testing"
	"time"
)

func sample(t *testing.T) *Project {
	t.Helper()
	repo, err := ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := New("p1", "infra", repo, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWorkingDirectoryAndToken(t *testing.T) {
	p := sample(t)
	if err := p.SetWorkingDirectory("terraform"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetWorkingDirectory("/tmp"); err == nil {
		t.Fatal("absolute directory accepted")
	}
	p.SetGitHubToken("secret")
	p.SetGitHubToken("")
	if p.Snapshot().GitHubToken != "secret" {
		t.Fatal("blank token replaced the stored token")
	}
	p.SetGitHubToken("next")
	if p.Snapshot().GitHubToken != "next" {
		t.Fatal("new token not stored")
	}
	p.RotateWebhookSecret("w1")
	p.RotateWebhookSecret("w2")
	if p.Snapshot().WebhookSecret != "w2" || p.Snapshot().DefaultBranch != "main" {
		t.Fatalf("%#v", p.Snapshot())
	}
}

func TestClaimAndRelease(t *testing.T) {
	p := sample(t)
	if err := p.ClaimActiveRun("run-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.ClaimActiveRun("run-1"); err != nil {
		t.Fatal("same run should reclaim")
	}
	if err := p.ClaimActiveRun("run-2"); err != ErrBusy {
		t.Fatalf("got %v", err)
	}
	if err := p.ReleaseActiveRun("run-2"); err != ErrActiveRunMismatch {
		t.Fatalf("release: %v", err)
	}
	if err := p.ReleaseActiveRun("run-1"); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().ActiveRunID != "" {
		t.Fatal("not released")
	}
	p.Lock()
	if !p.Snapshot().Locked {
		t.Fatal("not locked")
	}
	p.Unlock()
	if p.Snapshot().Locked {
		t.Fatal("still locked")
	}
}
