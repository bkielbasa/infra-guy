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

func TestFolderPolicyAndTagOverride(t *testing.T) {
	p := sample(t)
	p.SetFolderID("  folder-1  ")
	if p.Snapshot().FolderID != "folder-1" {
		t.Fatalf("folder id = %q", p.Snapshot().FolderID)
	}

	p.SetPolicyIDs([]string{" b ", "a", "b", "", "a"})
	snap := p.Snapshot()
	if len(snap.PolicyIDs) != 2 || snap.PolicyIDs[0] != "a" || snap.PolicyIDs[1] != "b" {
		t.Fatalf("policy ids = %#v", snap.PolicyIDs)
	}
	snap.PolicyIDs[0] = "mutated"
	if p.Snapshot().PolicyIDs[0] != "a" {
		t.Fatal("policy ids alias the snapshot")
	}

	if err := p.SetTagOverride(map[string]string{" env ": " prod "}); err != nil {
		t.Fatal(err)
	}
	snap = p.Snapshot()
	if !snap.TagsOverridden || snap.Tags["env"] != "prod" {
		t.Fatalf("%#v", snap.Tags)
	}
	if err := p.SetTagOverride(map[string]string{"bad key": "v"}); err == nil {
		t.Fatal("expected invalid tag")
	}
	if !p.Snapshot().TagsOverridden || p.Snapshot().Tags["env"] != "prod" {
		t.Fatal("failed override changed tags")
	}
	p.ClearTagOverride()
	snap = p.Snapshot()
	if snap.TagsOverridden || len(snap.Tags) != 0 {
		t.Fatalf("cleared tags = %#v overridden=%v", snap.Tags, snap.TagsOverridden)
	}
}

func TestSnapshotClonesEffectiveFields(t *testing.T) {
	p := sample(t)
	p.s.PolicyIDs = []string{"pol-a"}
	p.s.EffectiveTags = map[string]string{"env": "prod"}
	p.s.EffectivePolicies = []EffectivePolicy{{PolicyID: "pol-a", Source: "project"}}

	snap := p.Snapshot()
	snap.PolicyIDs[0] = "other"
	snap.EffectiveTags["env"] = "dev"
	snap.EffectivePolicies[0].PolicyID = "other"

	again := p.Snapshot()
	if again.PolicyIDs[0] != "pol-a" || again.EffectiveTags["env"] != "prod" || again.EffectivePolicies[0].PolicyID != "pol-a" {
		t.Fatalf("%#v", again)
	}
}

func TestTags(t *testing.T) {
	p := sample(t)
	if len(p.Snapshot().Tags) != 0 {
		t.Fatalf("expected empty tags, got %v", p.Snapshot().Tags)
	}
	tags, err := ParseTags(" env:prod , team:core\nregion:eu-central-1 ")
	if err != nil {
		t.Fatalf("ParseTags: %v", err)
	}
	if err := p.SetTags(tags); err != nil {
		t.Fatalf("SetTags: %v", err)
	}
	if got := FormatTags(p.Snapshot().Tags); got != "env:prod, region:eu-central-1, team:core" {
		t.Fatalf("FormatTags = %q", got)
	}
	list := TagList(p.Snapshot().Tags)
	if len(list) != 3 || list[0] != "env:prod" || list[1] != "region:eu-central-1" || list[2] != "team:core" {
		t.Fatalf("TagList = %v", list)
	}
	for _, bad := range []string{"invalid", ":val", "key:", "k ey:val", "key:v al"} {
		if _, err := ParseTags(bad); err == nil {
			t.Fatalf("expected error for invalid tag %q", bad)
		}
	}
}

func TestSetEngineValidatesConfig(t *testing.T) {
	p := sample(t)
	if err := p.SetEngine("helm", EngineConfig{ReleaseName: "payments", Chart: "./chart", HelmNamespace: "prod"}); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().Engine != EngineHelm || p.Snapshot().EngineConfig.Chart != "./chart" {
		t.Fatalf("%#v", p.Snapshot())
	}
	if err := p.SetEngine("helm", EngineConfig{ReleaseName: "Bad_Name", Chart: "nginx"}); err == nil {
		t.Fatal("expected invalid release name")
	}
	if err := p.SetEngine("kubernetes", EngineConfig{ManifestPath: "../secret"}); err == nil {
		t.Fatal("expected path escape to fail")
	}
	if err := p.SetEngine("kubernetes", EngineConfig{ToolVersion: "1.2"}); err == nil {
		t.Fatal("expected invalid tool version")
	}
	if err := p.SetEngine("kubernetes", EngineConfig{ToolVersion: "v1.31.2"}); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().EngineConfig.ToolVersion != "1.31.2" {
		t.Fatalf("tool version = %q", p.Snapshot().EngineConfig.ToolVersion)
	}
	if err := p.SetEngine("nope", EngineConfig{}); err == nil {
		t.Fatal("expected invalid engine")
	}
	if p.Snapshot().Engine != EngineKubernetes {
		t.Fatal("failed set should leave the previous engine")
	}
}
