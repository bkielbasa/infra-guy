package project

import (
	"errors"
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
	if err := p.SetDefaultBranch("develop"); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().DefaultBranch != "develop" {
		t.Fatalf("%#v", p.Snapshot())
	}
	if err := p.SetDefaultBranch("main"); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().DefaultBranch != "main" {
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

func TestClaimActiveRunRejectsLockedProject(t *testing.T) {
	p := sample(t)
	p.Lock()
	if err := p.ClaimActiveRun("run-1"); !errors.Is(err, ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}

func TestPlacement(t *testing.T) {
	p := sample(t)
	if err := p.SetPlacement("", 1); err == nil {
		t.Fatal("empty tenant accepted")
	}
	if err := p.SetPlacement("ten-1", 0); err == nil {
		t.Fatal("zero repo accepted")
	}
	if err := p.SetPlacement("ten-1", 99); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().TenantID != "ten-1" || p.Snapshot().GitHubRepoID != 99 {
		t.Fatalf("%#v", p.Snapshot())
	}
}

func TestTerraformVersion(t *testing.T) {
	p := sample(t)
	if p.Snapshot().TerraformVersion != DefaultTerraformVersion {
		t.Fatalf("default version = %q, want %q", p.Snapshot().TerraformVersion, DefaultTerraformVersion)
	}
	if err := p.SetTerraformVersion("1.10.5"); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().TerraformVersion != "1.10.5" {
		t.Fatalf("version = %q, want 1.10.5", p.Snapshot().TerraformVersion)
	}
	if err := p.SetTerraformVersion(""); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().TerraformVersion != DefaultTerraformVersion {
		t.Fatalf("empty reset = %q, want %q", p.Snapshot().TerraformVersion, DefaultTerraformVersion)
	}
	for _, bad := range []string{"latest", "1.9", "v1.9.8", "../1.9.8", "1.9.8-beta1", "1.9.8;id"} {
		if err := p.SetTerraformVersion(bad); err == nil {
			t.Fatalf("accepted invalid version %q", bad)
		}
	}
}

func TestApprovalPolicyID(t *testing.T) {
	p := sample(t)
	if p.ApprovalPolicyID() != "" || p.Snapshot().ApprovalPolicyID != "" {
		t.Fatalf("expected empty default approval policy id, got %q", p.ApprovalPolicyID())
	}
	p.ChangeApprovalPolicy("  pol-123  ")
	if p.ApprovalPolicyID() != "pol-123" || p.Snapshot().ApprovalPolicyID != "pol-123" {
		t.Fatalf("expected trimmed policy id pol-123, got %q", p.Snapshot().ApprovalPolicyID)
	}
	restored := Restore(p.Snapshot())
	if restored.ApprovalPolicyID() != "pol-123" {
		t.Fatalf("restored policy id = %q, want pol-123", restored.ApprovalPolicyID())
	}
	p.ChangeApprovalPolicy("   ")
	if p.ApprovalPolicyID() != "" {
		t.Fatalf("expected cleared policy id, got %q", p.ApprovalPolicyID())
	}
}

func TestRenameRepo(t *testing.T) {
	p := sample(t)
	later := time.Unix(200, 0).UTC()

	if err := p.RenameRepo("", "repo", later); err == nil {
		t.Fatal("expected error for empty owner")
	}
	if err := p.RenameRepo("owner", "  ", later); err == nil {
		t.Fatal("expected error for blank repo name")
	}

	if err := p.RenameRepo(" new-owner ", " new-repo ", later); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	snap := p.Snapshot()
	if snap.Repo.Owner != "new-owner" || snap.Repo.Name != "new-repo" {
		t.Fatalf("got %s/%s, want new-owner/new-repo", snap.Repo.Owner, snap.Repo.Name)
	}
	if !snap.UpdatedAt.Equal(later) {
		t.Fatalf("got UpdatedAt %v, want %v", snap.UpdatedAt, later)
	}
}

