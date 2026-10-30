package folderapp

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"infra-guy/internal/domain/folder"
	"infra-guy/internal/domain/project"
)

func TestEnsureRootIsStable(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	mem := newMemFolders()
	svc := &Service{Folders: mem, Now: func() time.Time { return now }}

	first, err := svc.EnsureRoot(ctx, "ten")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(first.ID) {
		t.Fatalf("id = %q, want 32 hex characters", first.ID)
	}
	if first.Name != "Default" || first.PassDown || !first.TagsSet || len(first.Tags) != 0 || first.ParentID != "" {
		t.Fatalf("root = %#v", first)
	}
	if !first.CreatedAt.Equal(now) || !first.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps = %s %s", first.CreatedAt, first.UpdatedAt)
	}
	second, err := svc.EnsureRoot(ctx, "ten")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("second id = %q, want %q", second.ID, first.ID)
	}
	if len(mem.ensureCalls) != 1 || mem.ensureCalls[0] != first.ID {
		t.Fatalf("ensure calls = %#v", mem.ensureCalls)
	}
}

func TestCreateRejectsDuplicateSiblingAndNinthLevel(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	mem := newMemFolders()
	mem.add(folder.Snapshot{ID: "root", TenantID: "ten", Name: "Default"})
	var n int
	svc := &Service{Folders: mem, Now: func() time.Time { return now }, NewID: func() string {
		n++
		return "fld-" + string(rune('0'+n))
	}}

	tags := map[string]string{"env": "dev"}
	ids := []string{"pol-b", "pol-a"}
	created, err := svc.Create(ctx, CreateInput{
		TenantID: "ten", ParentID: "root", Name: "  Foo  ", PassDown: true,
		PolicyIDs: ids, TagsSet: true, Tags: tags,
	})
	if err != nil {
		t.Fatal(err)
	}
	tags["env"] = "mutated"
	ids[0] = "mutated"
	if created.Name != "Foo" || !created.PassDown || !created.TagsSet || created.Tags["env"] != "dev" {
		t.Fatalf("created = %#v", created)
	}
	if len(created.PolicyIDs) != 2 || created.PolicyIDs[0] != "pol-b" || created.PolicyIDs[1] != "pol-a" {
		t.Fatalf("policy ids = %#v", created.PolicyIDs)
	}
	stored := mem.folders[created.ID]
	if stored.Tags["env"] != "dev" || stored.PolicyIDs[0] != "pol-b" {
		t.Fatal("create aliased tags or policy ids")
	}

	if _, err := svc.Create(ctx, CreateInput{TenantID: "ten", ParentID: "root", Name: "foo"}); err == nil || err.Error() != "a folder with that name already exists here" {
		t.Fatalf("duplicate sibling: %v", err)
	}
	if mem.saves != 1 {
		t.Fatalf("duplicate sibling was saved, saves = %d", mem.saves)
	}

	unset, err := svc.Create(ctx, CreateInput{
		TenantID: "ten", ParentID: "root", Name: "Other", TagsSet: false, Tags: map[string]string{"env": "dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if unset.TagsSet || len(unset.Tags) != 0 {
		t.Fatalf("tags_set false stored %#v", unset.Tags)
	}

	for i := 1; i <= 8; i++ {
		id := "d" + string(rune('0'+i))
		parent := ""
		if i > 1 {
			parent = "d" + string(rune('0'+i-1))
		}
		mem.add(folder.Snapshot{ID: id, TenantID: "ten", ParentID: parent, Name: id})
	}
	if _, err := svc.Create(ctx, CreateInput{TenantID: "ten", ParentID: "d7", Name: "wide"}); err != nil {
		t.Fatalf("depth 8: %v", err)
	}
	if _, err := svc.Create(ctx, CreateInput{TenantID: "ten", ParentID: "d8", Name: "too-deep"}); err == nil || err.Error() != "folder depth cannot exceed 8" {
		t.Fatalf("depth 9: %v", err)
	}
	if _, err := svc.Create(ctx, CreateInput{TenantID: "ten", ParentID: "missing", Name: "orphan"}); err == nil || err.Error() != "folder missing not found" {
		t.Fatalf("missing parent: %v", err)
	}
}

func TestCreateMapsUniqueViolation(t *testing.T) {
	ctx := context.Background()
	mem := newMemFolders()
	mem.add(folder.Snapshot{ID: "root", TenantID: "ten", Name: "Default"})
	mem.saveErr = &pgconn.PgError{Code: "23505"}
	svc := &Service{Folders: mem, NewID: func() string { return "fld-1" }}
	_, err := svc.Create(ctx, CreateInput{TenantID: "ten", ParentID: "root", Name: "Unique"})
	if err == nil || err.Error() != "a folder with that name already exists here" {
		t.Fatalf("unique violation: %v", err)
	}
	if _, ok := mem.folders["fld-1"]; ok {
		t.Fatal("unique violation was stored")
	}
}

func TestMoveRejectsDescendantAndSiblingName(t *testing.T) {
	ctx := context.Background()
	mem := newMemFolders()
	mem.add(folder.Snapshot{ID: "root", TenantID: "ten", Name: "Default", PassDown: true, PolicyIDs: []string{"keep"}})
	mem.add(folder.Snapshot{ID: "a", TenantID: "ten", ParentID: "root", Name: "A"})
	mem.add(folder.Snapshot{ID: "b", TenantID: "ten", ParentID: "a", Name: "B"})
	mem.add(folder.Snapshot{ID: "foo", TenantID: "ten", ParentID: "root", Name: "Foo"})
	mem.add(folder.Snapshot{ID: "holder", TenantID: "ten", ParentID: "root", Name: "Holder"})
	mem.add(folder.Snapshot{ID: "moving", TenantID: "ten", ParentID: "holder", Name: "foo", TagsSet: true, Tags: map[string]string{"env": "keep"}})
	svc := &Service{Folders: mem}

	if err := svc.Move(ctx, "ten", "a", "b"); err == nil || err.Error() != "folder cannot be moved under itself" {
		t.Fatalf("descendant: %v", err)
	}
	if mem.folders["a"].ParentID != "root" {
		t.Fatal("rejected move changed the parent")
	}
	if err := svc.Move(ctx, "ten", "moving", "root"); err == nil || err.Error() != "a folder with that name already exists here" {
		t.Fatalf("sibling name: %v", err)
	}
	if mem.saves != 0 {
		t.Fatalf("rejected move saved, saves = %d", mem.saves)
	}
	if err := svc.Move(ctx, "ten", "moving", "a"); err != nil {
		t.Fatal(err)
	}
	moved := mem.folders["moving"]
	if moved.ParentID != "a" || moved.Name != "foo" || moved.Tags["env"] != "keep" {
		t.Fatalf("moved = %#v", moved)
	}
	if err := svc.Move(ctx, "ten", "missing", "a"); err == nil || err.Error() != "folder missing not found" {
		t.Fatalf("missing: %v", err)
	}
}

func TestUpdateSettings(t *testing.T) {
	ctx := context.Background()
	mem := newMemFolders()
	mem.add(folder.Snapshot{
		ID: "root", TenantID: "ten", Name: "Default", TagsSet: true,
		Tags: map[string]string{"tier": "root"}, PolicyIDs: []string{"root-pol"},
	})
	mem.add(folder.Snapshot{
		ID: "child", TenantID: "ten", ParentID: "root", Name: "Child", PassDown: true,
		TagsSet: true, Tags: map[string]string{"env": "old"}, PolicyIDs: []string{"old-pol"},
	})
	mem.add(folder.Snapshot{ID: "taken", TenantID: "ten", ParentID: "root", Name: "Taken"})
	svc := &Service{Folders: mem}

	renamed, err := svc.UpdateSettings(ctx, SettingsInput{
		ID: "child", TenantID: "ten", Name: "Renamed", UpdateTags: false,
		PassDown: false, PolicyIDs: []string{"nope"}, TagsSet: false, Tags: map[string]string{"env": "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "Renamed" || !renamed.PassDown || !renamed.TagsSet || renamed.Tags["env"] != "old" || renamed.PolicyIDs[0] != "old-pol" {
		t.Fatalf("rename replaced settings: %#v", renamed)
	}
	if _, err := svc.UpdateSettings(ctx, SettingsInput{ID: "child", TenantID: "ten", Name: "taken"}); err == nil || err.Error() != "a folder with that name already exists here" {
		t.Fatalf("rename clash: %v", err)
	}
	if mem.folders["child"].Name != "Renamed" {
		t.Fatalf("rejected rename changed name to %q", mem.folders["child"].Name)
	}

	updated, err := svc.UpdateSettings(ctx, SettingsInput{
		ID: "child", TenantID: "ten", Name: "Renamed", UpdateTags: true, PassDown: false,
		PolicyIDs: []string{"new-pol"}, TagsSet: true, Tags: map[string]string{"env": "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.PassDown || !updated.TagsSet || updated.Tags["env"] != "new" || len(updated.PolicyIDs) != 1 || updated.PolicyIDs[0] != "new-pol" {
		t.Fatalf("update = %#v", updated)
	}

	cleared, err := svc.UpdateSettings(ctx, SettingsInput{
		ID: "child", TenantID: "ten", Name: "Renamed", UpdateTags: true, TagsSet: false,
		Tags: map[string]string{"env": "ignored"}, PassDown: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.TagsSet || len(cleared.Tags) != 0 || !cleared.PassDown || len(cleared.PolicyIDs) != 0 {
		t.Fatalf("cleared tags = %#v", cleared)
	}

	if _, err := svc.UpdateSettings(ctx, SettingsInput{ID: "root", TenantID: "ten", Name: "Default", UpdateTags: true, TagsSet: false}); err == nil || err.Error() != "the root folder must set its own tags" {
		t.Fatalf("root tags: %v", err)
	}
	if mem.folders["root"].Tags["tier"] != "root" || !mem.folders["root"].TagsSet {
		t.Fatalf("root changed: %#v", mem.folders["root"])
	}
	if _, err := svc.UpdateSettings(ctx, SettingsInput{ID: "missing", TenantID: "ten", Name: "Nope"}); err == nil || err.Error() != "folder missing not found" {
		t.Fatalf("missing: %v", err)
	}
}

func TestDeleteReparentsProjectsAndRejectsRootAndNameClash(t *testing.T) {
	ctx := context.Background()
	mem := newMemFolders()
	mem.add(folder.Snapshot{ID: "root", TenantID: "ten", Name: "Default"})
	mem.add(folder.Snapshot{ID: "parent", TenantID: "ten", ParentID: "root", Name: "Parent"})
	mem.add(folder.Snapshot{ID: "child", TenantID: "ten", ParentID: "parent", Name: "Child"})
	direct := mustProject(t, "direct", "ten", "parent", nil, map[string]string{"env": "prod"})
	directSnap := direct.Snapshot()
	directSnap.PolicyIDs = []string{"pol-b", "pol-a"}
	direct = project.Restore(directSnap)
	nested := mustProject(t, "nested", "ten", "child", []string{"pol-child"}, map[string]string{"team": "core"})
	mem.projects[direct.Snapshot().ID] = direct
	mem.projects[nested.Snapshot().ID] = nested
	svc := &Service{Folders: mem}

	if _, err := svc.Delete(ctx, "ten", "root"); err == nil || err.Error() != "the root folder cannot be deleted" {
		t.Fatalf("root: %v", err)
	}
	if mem.deletes != 0 {
		t.Fatal("root delete called the repository")
	}

	parentID, err := svc.Delete(ctx, "ten", "parent")
	if err != nil {
		t.Fatal(err)
	}
	if parentID != "root" || mem.deletes != 1 || mem.deleteParent != "root" {
		t.Fatalf("parentID=%q deletes=%d deleteParent=%q", parentID, mem.deletes, mem.deleteParent)
	}
	gotDirect := mem.projects["direct"].Snapshot()
	if gotDirect.FolderID != "root" || gotDirect.TagsOverridden || len(gotDirect.Tags) != 0 {
		t.Fatalf("direct project = %#v", gotDirect)
	}
	if len(gotDirect.PolicyIDs) != 2 || gotDirect.PolicyIDs[0] != "pol-b" || gotDirect.PolicyIDs[1] != "pol-a" {
		t.Fatalf("direct policies = %#v", gotDirect.PolicyIDs)
	}
	gotNested := mem.projects["nested"].Snapshot()
	if !gotNested.TagsOverridden || gotNested.Tags["team"] != "core" || gotNested.FolderID != "child" {
		t.Fatalf("nested project = %#v", gotNested)
	}
	if len(gotNested.PolicyIDs) != 1 || gotNested.PolicyIDs[0] != "pol-child" {
		t.Fatalf("nested policies = %#v", gotNested.PolicyIDs)
	}
	if mem.folders["child"].ParentID != "root" {
		t.Fatalf("child parent = %q", mem.folders["child"].ParentID)
	}
	if _, ok := mem.folders["parent"]; ok {
		t.Fatal("deleted folder remains")
	}

	mem.add(folder.Snapshot{ID: "keep", TenantID: "ten", ParentID: "root", Name: "Shared"})
	mem.add(folder.Snapshot{ID: "doomed", TenantID: "ten", ParentID: "root", Name: "Doomed"})
	mem.add(folder.Snapshot{ID: "clash", TenantID: "ten", ParentID: "doomed", Name: "shared"})
	before := mem.deletes
	if _, err := svc.Delete(ctx, "ten", "doomed"); err == nil || err.Error() != "a folder with that name already exists here" {
		t.Fatalf("sibling clash: %v", err)
	}
	if mem.deletes != before {
		t.Fatal("name clash called the repository")
	}
	if _, ok := mem.folders["doomed"]; !ok {
		t.Fatal("rejected delete removed the folder")
	}
	if _, err := svc.Delete(ctx, "ten", "missing"); err == nil || err.Error() != "folder missing not found" {
		t.Fatalf("missing: %v", err)
	}
}

func TestEffectiveIncludesAncestorPolicyOnlyWhenPassDown(t *testing.T) {
	ctx := context.Background()
	mem := newMemFolders()
	mem.add(folder.Snapshot{ID: "root", TenantID: "ten", Name: "Default", TagsSet: true, PolicyIDs: []string{"root-pol"}})
	mem.add(folder.Snapshot{ID: "parent", TenantID: "ten", ParentID: "root", Name: "Parent", PolicyIDs: []string{"parent-pol"}})
	mem.add(folder.Snapshot{ID: "child", TenantID: "ten", ParentID: "parent", Name: "Child", PolicyIDs: []string{"child-pol"}})
	svc := &Service{Folders: mem}
	p := project.Snapshot{TenantID: "ten", FolderID: "child", PolicyIDs: []string{"proj-pol"}}

	got, err := svc.Effective(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if sources := policySources(got); sources["parent-pol"] || sources["root-pol"] || !sources["child-pol"] || !sources["proj-pol"] {
		t.Fatalf("pass-down off: %#v", got.EffectivePolicies)
	}

	parent := mem.folders["parent"]
	parent.PassDown = true
	mem.folders["parent"] = parent
	got, err = svc.Effective(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if sources := policySources(got); !sources["parent-pol"] || sources["root-pol"] {
		t.Fatalf("parent pass-down: %#v", got.EffectivePolicies)
	}

	root := mem.folders["root"]
	root.PassDown = true
	mem.folders["root"] = root
	got, err = svc.Effective(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if sources := policySources(got); !sources["root-pol"] || !sources["parent-pol"] {
		t.Fatalf("root pass-down: %#v", got.EffectivePolicies)
	}
}

func policySources(p project.Snapshot) map[string]bool {
	out := map[string]bool{}
	for _, pol := range p.EffectivePolicies {
		out[pol.PolicyID] = true
	}
	return out
}

type memFolders struct {
	folders      map[string]folder.Snapshot
	projects     map[project.ID]*project.Project
	ensureCalls  []string
	saves        int
	saveErr      error
	deletes      int
	deleteParent string
}

func newMemFolders() *memFolders {
	return &memFolders{
		folders:  map[string]folder.Snapshot{},
		projects: map[project.ID]*project.Project{},
	}
}

func (m *memFolders) add(s folder.Snapshot) {
	if s.TenantID == "" {
		s.TenantID = "ten"
	}
	m.folders[s.ID] = s
}

func (m *memFolders) ListByTenant(_ context.Context, tenantID string) ([]folder.Snapshot, error) {
	var out []folder.Snapshot
	for _, f := range m.folders {
		if f.TenantID == tenantID {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *memFolders) Save(_ context.Context, s folder.Snapshot) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.saves++
	m.folders[s.ID] = s
	return nil
}

func (m *memFolders) EnsureRoot(_ context.Context, tenantID, id string, now time.Time) (folder.Snapshot, error) {
	m.ensureCalls = append(m.ensureCalls, id)
	s := folder.Snapshot{
		ID: id, TenantID: tenantID, Name: "Default", PassDown: false, TagsSet: true,
		Tags: map[string]string{}, CreatedAt: now, UpdatedAt: now,
	}
	m.folders[id] = s
	return s, nil
}

func (m *memFolders) DeleteAndReparent(_ context.Context, tenantID, folderID, parentID string, now time.Time) error {
	m.deletes++
	m.deleteParent = parentID
	for id, p := range m.projects {
		snap := p.Snapshot()
		if snap.TenantID == tenantID && snap.FolderID == folderID {
			p.SetFolderID(parentID)
			p.ClearTagOverride()
			m.projects[id] = p
		}
	}
	for id, f := range m.folders {
		if f.TenantID == tenantID && f.ParentID == folderID {
			f.ParentID = parentID
			f.UpdatedAt = now
			m.folders[id] = f
		}
	}
	delete(m.folders, folderID)
	return nil
}

func mustProject(t *testing.T, id, tenantID, folderID string, policies []string, tags map[string]string) *project.Project {
	t.Helper()
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New(project.ID(id), id, repo, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetPlacement(tenantID, 1); err != nil {
		t.Fatal(err)
	}
	p.SetFolderID(folderID)
	p.SetPolicyIDs(policies)
	if err := p.SetTagOverride(tags); err != nil {
		t.Fatal(err)
	}
	return p
}
