package folder

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"infra-guy/internal/domain/project"
)

func TestParseName(t *testing.T) {
	if _, err := ParseName("   "); err == nil || err.Error() != "folder name is required" {
		t.Fatalf("blank: %v", err)
	}
	if _, err := ParseName(""); err == nil || err.Error() != "folder name is required" {
		t.Fatalf("empty: %v", err)
	}
	if _, err := ParseName("acme/prod"); err == nil || err.Error() != "folder name cannot contain a slash" {
		t.Fatalf("slash: %v", err)
	}
	if _, err := ParseName("bad\x01name"); err == nil || err.Error() != "folder name contains invalid characters" {
		t.Fatalf("control: %v", err)
	}
	if _, err := ParseName(strings.Repeat("a", 81)); err == nil || err.Error() != "folder name is too long" {
		t.Fatalf("81 characters: %v", err)
	}
	got, err := ParseName("  " + strings.Repeat("b", 80) + "  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.Repeat("b", 80) {
		t.Fatalf("80 characters: got %q", got)
	}
	got, err = ParseName("  Platform  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Platform" {
		t.Fatalf("trimmed name = %q", got)
	}
}

func TestValidateMove(t *testing.T) {
	folders := make([]Snapshot, 0, 11)
	parent := ""
	for i := 1; i <= 8; i++ {
		id := "d" + strconv.Itoa(i)
		folders = append(folders, Snapshot{ID: id, ParentID: parent, Name: id})
		parent = id
	}
	folders = append(folders,
		Snapshot{ID: "x", ParentID: "d1", Name: "x"},
		Snapshot{ID: "a", ParentID: "d1", Name: "a"},
		Snapshot{ID: "b", ParentID: "a", Name: "b"},
	)

	if err := ValidateMove(folders, "x", "d7"); err != nil {
		t.Fatalf("depth 8: %v", err)
	}
	if err := ValidateMove(folders, "x", "d8"); err == nil || err.Error() != "folder depth cannot exceed 8" {
		t.Fatalf("depth 9: %v", err)
	}
	if err := ValidateMove(folders, "a", "d6"); err != nil {
		t.Fatalf("subtree depth 8: %v", err)
	}
	if err := ValidateMove(folders, "a", "d7"); err == nil || err.Error() != "folder depth cannot exceed 8" {
		t.Fatalf("subtree depth 9: %v", err)
	}
	if err := ValidateMove(folders, "d3", "d3"); err == nil || err.Error() != "folder cannot be moved under itself" {
		t.Fatalf("self: %v", err)
	}
	if err := ValidateMove(folders, "d3", "d6"); err == nil || err.Error() != "folder cannot be moved under itself" {
		t.Fatalf("descendant: %v", err)
	}
	if err := ValidateMove(folders, "d1", "d2"); err == nil || err.Error() != "the root folder cannot be moved" {
		t.Fatalf("root: %v", err)
	}
	if err := ValidateMove(folders, "d1", ""); err == nil || err.Error() != "the root folder cannot be moved" {
		t.Fatalf("root empty parent: %v", err)
	}
	if err := ValidateMove(folders, "x", "  "); err == nil || err.Error() != "the root folder cannot be moved" {
		t.Fatalf("empty new parent: %v", err)
	}
}

func TestResolve(t *testing.T) {
	folders := []Snapshot{
		{ID: "root", ParentID: "", Name: "Default", TagsSet: true, Tags: map[string]string{"tier": "root"}, PolicyIDs: []string{"root-pol"}},
		{ID: "parent", ParentID: "root", Name: "Parent", TagsSet: true, Tags: map[string]string{"env": "parent"}, PolicyIDs: []string{"parent-pol"}},
		{ID: "child", ParentID: "parent", Name: "Child", TagsSet: true, Tags: map[string]string{"env": "child"}, PolicyIDs: []string{"pol-a", "folder-pol"}},
		{ID: "grand", ParentID: "child", Name: "Grand"},
	}
	stored := map[string]string{"team": "stored"}
	ids := []string{"pol-b", "pol-a"}
	p := project.Snapshot{FolderID: "child", PolicyIDs: ids, TagsOverridden: false, Tags: stored}

	got, err := Resolve(p, folders)
	if err != nil {
		t.Fatal(err)
	}
	want := []project.EffectivePolicy{
		{PolicyID: "pol-a", Source: "project"},
		{PolicyID: "pol-b", Source: "project"},
		{PolicyID: "folder-pol", Source: "child"},
	}
	if !reflect.DeepEqual(got.EffectivePolicies, want) {
		t.Fatalf("policies = %#v, want %#v", got.EffectivePolicies, want)
	}
	if got.EffectiveTags["env"] != "child" || len(got.EffectiveTags) != 1 {
		t.Fatalf("immediate tags = %#v", got.EffectiveTags)
	}
	if got.Tags["team"] != "stored" || got.TagsOverridden {
		t.Fatalf("stored tags changed: %#v overridden=%v", got.Tags, got.TagsOverridden)
	}
	if len(got.PolicyIDs) != 2 || got.PolicyIDs[0] != "pol-b" || got.PolicyIDs[1] != "pol-a" {
		t.Fatalf("policy ids changed: %#v", got.PolicyIDs)
	}
	stored["team"] = "mutated"
	ids[0] = "mutated"
	if got.Tags["team"] != "stored" || got.PolicyIDs[0] != "pol-b" {
		t.Fatal("resolve aliased stored tags or policy ids")
	}
	stored["team"] = "stored"
	ids[0] = "pol-b"

	folders[1].PassDown = true
	got, err = Resolve(p, folders)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, project.EffectivePolicy{PolicyID: "parent-pol", Source: "parent"})
	if !reflect.DeepEqual(got.EffectivePolicies, want) {
		t.Fatalf("parent pass-down = %#v, want %#v", got.EffectivePolicies, want)
	}

	grand := project.Snapshot{FolderID: "grand", PolicyIDs: []string{"pol-b", "pol-a"}}
	got, err = Resolve(grand, folders)
	if err != nil {
		t.Fatal(err)
	}
	wantGrand := []project.EffectivePolicy{
		{PolicyID: "pol-a", Source: "project"},
		{PolicyID: "pol-b", Source: "project"},
		{PolicyID: "parent-pol", Source: "parent"},
	}
	if !reflect.DeepEqual(got.EffectivePolicies, wantGrand) {
		t.Fatalf("grandchild pass-down = %#v, want %#v", got.EffectivePolicies, wantGrand)
	}

	folders[0].PassDown = true
	got, err = Resolve(grand, folders)
	if err != nil {
		t.Fatal(err)
	}
	wantGrand = append(wantGrand, project.EffectivePolicy{PolicyID: "root-pol", Source: "root"})
	if !reflect.DeepEqual(got.EffectivePolicies, wantGrand) {
		t.Fatalf("root pass-down = %#v, want %#v", got.EffectivePolicies, wantGrand)
	}

	folders[2].Tags = map[string]string{}
	folders[2].TagsSet = true
	got, err = Resolve(p, folders)
	if err != nil {
		t.Fatal(err)
	}
	if got.EffectiveTags == nil || len(got.EffectiveTags) != 0 {
		t.Fatalf("nearer empty tags = %#v", got.EffectiveTags)
	}

	folders[2].TagsSet = false
	folders[2].Tags = map[string]string{"env": "child"}
	got, err = Resolve(p, folders)
	if err != nil {
		t.Fatal(err)
	}
	if got.EffectiveTags["env"] != "parent" || len(got.EffectiveTags) != 1 {
		t.Fatalf("tags_set false = %#v", got.EffectiveTags)
	}

	folders[1].PassDown = false
	folders[0].PassDown = false
	got, err = Resolve(p, folders)
	if err != nil {
		t.Fatal(err)
	}
	if got.EffectiveTags == nil || len(got.EffectiveTags) != 0 {
		t.Fatalf("pass-down off tags = %#v", got.EffectiveTags)
	}
	if len(got.EffectivePolicies) != 3 || got.EffectivePolicies[2].Source != "child" {
		t.Fatalf("pass-down off policies = %#v", got.EffectivePolicies)
	}

	p.TagsOverridden = true
	p.Tags = map[string]string{"team": "core"}
	folders[2].TagsSet = true
	folders[2].Tags = map[string]string{"env": "child"}
	got, err = Resolve(p, folders)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.EffectiveTags) != 1 || got.EffectiveTags["team"] != "core" {
		t.Fatalf("override tags = %#v", got.EffectiveTags)
	}
	if !got.TagsOverridden || got.Tags["team"] != "core" {
		t.Fatalf("stored override changed: %#v", got.Tags)
	}
	p.Tags["team"] = "mutated"
	if got.Tags["team"] != "core" || got.EffectiveTags["team"] != "core" {
		t.Fatal("override aliased input tags")
	}

	if _, err := Resolve(project.Snapshot{FolderID: "missing"}, folders); err == nil || err.Error() != "folder missing not found" {
		t.Fatalf("missing folder: %v", err)
	}
	folders[2].ParentID = "ghost"
	if _, err := Resolve(project.Snapshot{FolderID: "child"}, folders); err == nil || err.Error() != "folder ghost not found" {
		t.Fatalf("missing ancestor: %v", err)
	}
}
