package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/folder"
	"infra-guy/internal/domain/policy"
	"infra-guy/internal/domain/project"
)

func TestProjectFolderMigration(t *testing.T) {
	ctx := context.Background()
	base := openTestPool(t, ctx)
	pool := schemaPool(t, ctx, base, fmt.Sprintf("pfmig_%d", time.Now().UnixNano()))

	m, cleanup, err := NewMigrator(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if err := m.Migrate(17); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 17: %v", err)
	}

	hasApproval := projectsColumnExists(t, ctx, pool, "approval_policy_id")
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantID := "ten-pf-" + suffix
	policyID := "pol-pf-" + suffix
	projectID := "proj-pf-" + suffix
	if hasApproval {
		seedPreMigrationProject(t, ctx, pool, tenantID, policyID, projectID)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	if projectsColumnExists(t, ctx, pool, "approval_policy_id") {
		t.Fatal("projects.approval_policy_id still exists")
	}
	if !hasApproval {
		t.Fatal("pre-migration schema had no projects.approval_policy_id to migrate")
	}

	var roots int
	var rootID, rootName string
	var passDown, tagsSet bool
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM folders WHERE tenant_id = $1 AND parent_id IS NULL`, tenantID).Scan(&roots); err != nil {
		t.Fatal(err)
	}
	if roots != 1 {
		t.Fatalf("root folders = %d, want 1", roots)
	}
	if err := pool.QueryRow(ctx, `
		SELECT id, name, pass_down, tags_set
		FROM folders
		WHERE tenant_id = $1 AND parent_id IS NULL`, tenantID).Scan(&rootID, &rootName, &passDown, &tagsSet); err != nil {
		t.Fatal(err)
	}
	if rootName != "Default" || passDown || !tagsSet {
		t.Fatalf("root = name %q pass_down %v tags_set %v", rootName, passDown, tagsSet)
	}

	var folderID string
	var tagsOverridden bool
	var tagsRaw []byte
	if err := pool.QueryRow(ctx, `
		SELECT folder_id, tags_overridden, tags FROM projects WHERE id = $1`, projectID).Scan(&folderID, &tagsOverridden, &tagsRaw); err != nil {
		t.Fatal(err)
	}
	if folderID != rootID || !tagsOverridden {
		t.Fatalf("project folder_id=%q tags_overridden=%v, root=%q", folderID, tagsOverridden, rootID)
	}
	var tags map[string]string
	if err := json.Unmarshal(tagsRaw, &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags["env"] != "prod" {
		t.Fatalf("project tags = %#v, want env:prod", tags)
	}

	var storedPolicy string
	if err := pool.QueryRow(ctx, `
		SELECT policy_id FROM project_policies WHERE project_id = $1`, projectID).Scan(&storedPolicy); err != nil {
		t.Fatal(err)
	}
	if storedPolicy != policyID {
		t.Fatalf("project_policies policy_id = %q, want %q", storedPolicy, policyID)
	}

	multiID := projectID + "-multi"
	if _, err := pool.Exec(ctx, `
		INSERT INTO projects (
			id, tenant_id, github_repo_id, name, repo_owner, repo_name, default_branch,
			folder_id, tags_overridden, tags, created_at, updated_at
		) VALUES ($1,$2,43,'multi','bklebasa','infra','main',$3,false,'{}'::jsonb,$4,$4)`,
		multiID, tenantID, rootID, time.Unix(1_700_000_000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO project_policies (project_id, policy_id) VALUES ($1, $2)`, multiID, policyID); err != nil {
		t.Fatal(err)
	}
	secondID := policyID + "-b"
	second, err := policy.New(policy.NewInput{
		ID: policy.ID(secondID), TenantID: tenantID, Name: "Second " + secondID,
		Rego: "package infraguy\ndefault allow = false\n", Now: time.Unix(1_700_000_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewPolicyRepo(pool).Insert(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO project_policies (project_id, policy_id) VALUES ($1, $2)`, multiID, secondID); err != nil {
		t.Fatal(err)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if !projectsColumnExists(t, ctx, pool, "approval_policy_id") || projectsColumnExists(t, ctx, pool, "folder_id") || projectsColumnExists(t, ctx, pool, "tags_overridden") {
		t.Fatal("down migration did not restore approval_policy_id and drop folder columns")
	}
	var restored *string
	if err := pool.QueryRow(ctx, `SELECT approval_policy_id FROM projects WHERE id = $1`, projectID).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if restored == nil || *restored != policyID {
		t.Fatalf("single policy restored as %v, want %s", restored, policyID)
	}
	var multi *string
	if err := pool.QueryRow(ctx, `SELECT approval_policy_id FROM projects WHERE id = $1`, multiID).Scan(&multi); err != nil {
		t.Fatal(err)
	}
	if multi != nil {
		t.Fatalf("project with two policies restored approval_policy_id %q", *multi)
	}
}

func TestProjectFolderRepo(t *testing.T) {
	ctx := context.Background()
	base := openTestPool(t, ctx)
	pool := schemaPool(t, ctx, base, fmt.Sprintf("pfrepo_%d", time.Now().UnixNano()))
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_700_000_100, 0).UTC()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantID := "ten-fr-" + suffix
	otherTenant := "ten-fr-other-" + suffix
	accountID := time.Now().UnixNano()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, 'acme', $2, 'organization', now(), now()), ($3, 'other', $4, 'organization', now(), now())`,
		tenantID, accountID, otherTenant, accountID+1); err != nil {
		t.Fatal(err)
	}

	folders := NewFolderRepo(pool)
	root, err := folders.EnsureRoot(ctx, tenantID, "root-"+suffix, now)
	if err != nil {
		t.Fatal(err)
	}
	if root.ID != "root-"+suffix || root.TenantID != tenantID || root.ParentID != "" || root.Name != "Default" || root.PassDown || !root.TagsSet || len(root.PolicyIDs) != 0 || !root.CreatedAt.Equal(now) || !root.UpdatedAt.Equal(now) {
		t.Fatalf("created root: %#v", root)
	}
	root.Name = "Renamed"
	root.Tags = map[string]string{"tier": "root"}
	root.TagsSet = true
	root.UpdatedAt = now.Add(time.Minute)
	if err := folders.Save(ctx, root); err != nil {
		t.Fatal(err)
	}
	again, err := folders.EnsureRoot(ctx, tenantID, "ignored-"+suffix, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != root.ID || again.Name != "Renamed" || again.Tags["tier"] != "root" {
		t.Fatalf("EnsureRoot changed existing root: %#v", again)
	}

	polA := insertTestPolicy(t, ctx, pool, tenantID, "pol-a-"+suffix, now)
	polB := insertTestPolicy(t, ctx, pool, tenantID, "pol-b-"+suffix, now)
	parent := folder.Snapshot{
		ID: "parent-" + suffix, TenantID: tenantID, ParentID: root.ID, Name: "Parent",
		PassDown: true, TagsSet: true, Tags: map[string]string{"env": "parent"},
		PolicyIDs: []string{polB, polA}, CreatedAt: now, UpdatedAt: now,
	}
	if err := folders.Save(ctx, parent); err != nil {
		t.Fatal(err)
	}
	parent.PolicyIDs = []string{polA}
	parent.Name = "Parent Renamed"
	parent.UpdatedAt = now.Add(2 * time.Minute)
	if err := folders.Save(ctx, parent); err != nil {
		t.Fatal(err)
	}
	gotParent, err := folders.Get(ctx, tenantID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotParent.Name != "Parent Renamed" || gotParent.ParentID != root.ID || !gotParent.PassDown || !gotParent.TagsSet || gotParent.Tags["env"] != "parent" || len(gotParent.PolicyIDs) != 1 || gotParent.PolicyIDs[0] != polA {
		t.Fatalf("saved parent: %#v", gotParent)
	}
	if _, err := folders.Get(ctx, otherTenant, parent.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-tenant get err = %v", err)
	}

	child := folder.Snapshot{
		ID: "child-" + suffix, TenantID: tenantID, ParentID: parent.ID, Name: "Child",
		TagsSet: false, Tags: map[string]string{"team": "core"}, PolicyIDs: []string{polB},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := folders.Save(ctx, child); err != nil {
		t.Fatal(err)
	}
	if _, err := folders.EnsureRoot(ctx, otherTenant, "other-root-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	listed, err := folders.ListByTenant(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("ListByTenant len = %d, want 3 (%#v)", len(listed), listed)
	}
	byID := map[string]folder.Snapshot{}
	for _, f := range listed {
		byID[f.ID] = f
	}
	if byID[child.ID].ParentID != parent.ID || len(byID[child.ID].PolicyIDs) != 1 || byID[child.ID].PolicyIDs[0] != polB {
		t.Fatalf("listed child: %#v", byID[child.ID])
	}

	projects := NewProjectRepo(pool)
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	direct, err := project.New(project.ID("proj-direct-"+suffix), "direct", repo, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := direct.SetPlacement(tenantID, 7); err != nil {
		t.Fatal(err)
	}
	direct.SetFolderID(parent.ID)
	if err := direct.SetTagOverride(map[string]string{"env": "prod"}); err != nil {
		t.Fatal(err)
	}
	direct.SetPolicyIDs([]string{polB, polA})
	direct.ChangeApprovalPolicy("legacy-policy")
	snap := direct.Snapshot()
	snap.EffectiveTags = map[string]string{"env": "leaked"}
	snap.EffectivePolicies = []project.EffectivePolicy{{PolicyID: polA, Source: "project"}}
	direct = project.Restore(snap)
	if err := projects.Save(ctx, direct); err != nil {
		t.Fatal(err)
	}
	gotDirect, err := projects.Get(ctx, direct.Snapshot().ID)
	if err != nil || gotDirect == nil {
		t.Fatal(err)
	}
	ds := gotDirect.Snapshot()
	if ds.FolderID != parent.ID || !ds.TagsOverridden || ds.Tags["env"] != "prod" || ds.ApprovalPolicyID != "" || len(ds.EffectiveTags) != 0 || len(ds.EffectivePolicies) != 0 || len(ds.PolicyIDs) != 2 || ds.PolicyIDs[0] != polA || ds.PolicyIDs[1] != polB {
		t.Fatalf("direct project: %#v", ds)
	}
	listedProjects, err := projects.List(ctx, tenantID)
	if err != nil || len(listedProjects) != 1 || len(listedProjects[0].Snapshot().PolicyIDs) != 2 {
		t.Fatalf("list projects: %v %#v", err, listedProjects)
	}

	nested, err := project.New(project.ID("proj-nested-"+suffix), "nested", repo, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := nested.SetPlacement(tenantID, 8); err != nil {
		t.Fatal(err)
	}
	nested.SetFolderID(child.ID)
	if err := nested.SetTagOverride(map[string]string{"team": "core"}); err != nil {
		t.Fatal(err)
	}
	nested.SetPolicyIDs([]string{polB})
	if err := projects.Save(ctx, nested); err != nil {
		t.Fatal(err)
	}

	movedAt := now.Add(5 * time.Minute)
	if err := folders.DeleteAndReparent(ctx, tenantID, parent.ID, root.ID, movedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := folders.Get(ctx, tenantID, parent.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted folder get err = %v", err)
	}
	gotChild, err := folders.Get(ctx, tenantID, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.ParentID != root.ID || !gotChild.UpdatedAt.Equal(movedAt) || len(gotChild.PolicyIDs) != 1 {
		t.Fatalf("reparented child: %#v", gotChild)
	}
	var parentPolicies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM folder_policies WHERE folder_id = $1`, parent.ID).Scan(&parentPolicies); err != nil {
		t.Fatal(err)
	}
	if parentPolicies != 0 {
		t.Fatalf("deleted folder policies = %d", parentPolicies)
	}

	gotDirect, err = projects.Get(ctx, direct.Snapshot().ID)
	if err != nil || gotDirect == nil {
		t.Fatal(err)
	}
	ds = gotDirect.Snapshot()
	if ds.FolderID != root.ID || ds.TagsOverridden || len(ds.Tags) != 0 || !ds.UpdatedAt.Equal(movedAt) || len(ds.PolicyIDs) != 2 {
		t.Fatalf("moved direct project: %#v", ds)
	}
	var directTags string
	if err := pool.QueryRow(ctx, `SELECT tags::text FROM projects WHERE id = $1`, direct.Snapshot().ID).Scan(&directTags); err != nil {
		t.Fatal(err)
	}
	if directTags != "{}" {
		t.Fatalf("direct project tags = %s, want {}", directTags)
	}
	gotNested, err := projects.Get(ctx, nested.Snapshot().ID)
	if err != nil || gotNested == nil {
		t.Fatal(err)
	}
	ns := gotNested.Snapshot()
	if ns.FolderID != child.ID || !ns.TagsOverridden || ns.Tags["team"] != "core" || len(ns.PolicyIDs) != 1 || ns.PolicyIDs[0] != polB || !ns.UpdatedAt.Equal(now) {
		t.Fatalf("nested project changed: %#v", ns)
	}

	direct.ClearTagOverride()
	direct.SetFolderID(root.ID)
	direct.SetPolicyIDs([]string{polB})
	if err := projects.Save(ctx, direct); err != nil {
		t.Fatal(err)
	}
	gotDirect, err = projects.Get(ctx, direct.Snapshot().ID)
	if err != nil || gotDirect == nil {
		t.Fatal(err)
	}
	ds = gotDirect.Snapshot()
	if ds.TagsOverridden || len(ds.Tags) != 0 || len(ds.PolicyIDs) != 1 || ds.PolicyIDs[0] != polB || ds.ApprovalPolicyID != "" {
		t.Fatalf("cleared override: %#v", ds)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, 'race', $2, 'organization', now(), now())`, "ten-race-"+suffix, accountID+2); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	ids := make([]string, 8)
	errs := make([]error, 8)
	for i := 0; i < len(ids); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := folders.EnsureRoot(ctx, "ten-race-"+suffix, fmt.Sprintf("race-%d-%s", i, suffix), now)
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = got.ID
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureRoot %d: %v", i, err)
		}
		if ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("concurrent roots = %#v", ids)
		}
	}
}

func insertTestPolicy(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, id string, now time.Time) string {
	t.Helper()
	pol, err := policy.New(policy.NewInput{
		ID: policy.ID(id), TenantID: tenantID, Name: id, Rego: "package infraguy\ndefault allow = true\n", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewPolicyRepo(pool).Insert(ctx, pol); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedPreMigrationProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, policyID, projectID string) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0).UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, 'acme', $2, 'organization', now(), now())`, tenantID, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	pol, err := policy.New(policy.NewInput{
		ID:       policy.ID(policyID),
		TenantID: tenantID,
		Name:     "Prod Approvers " + policyID,
		Rego:     "package infraguy\ndefault allow = true\n",
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewPolicyRepo(pool).Insert(ctx, pol); err != nil {
		t.Fatal(err)
	}
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New(project.ID(projectID), "infra", repo, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetPlacement(tenantID, 42); err != nil {
		t.Fatal(err)
	}
	if err := p.SetTags(map[string]string{"env": "prod"}); err != nil {
		t.Fatal(err)
	}
	p.ChangeApprovalPolicy(policyID)
	s := p.Snapshot()
	tagsJSON, err := json.Marshal(s.Tags)
	if err != nil {
		t.Fatal(err)
	}
	// approval_policy_id exists only on the pre-migration schema. ProjectRepo
	// stores PolicyIDs, so this legacy row is inserted directly.
	if _, err := pool.Exec(ctx, `
		INSERT INTO projects (
			id, tenant_id, github_repo_id, name, repo_owner, repo_name, default_branch,
			approval_policy_id, tags, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11)`,
		s.ID, s.TenantID, s.GitHubRepoID, s.Name, s.Repo.Owner, s.Repo.Name, s.DefaultBranch,
		s.ApprovalPolicyID, string(tagsJSON), s.CreatedAt, s.UpdatedAt,
	); err != nil {
		t.Fatal(err)
	}
}

func projectsColumnExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, column string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = 'projects'
			  AND column_name = $1
		)`, column).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func schemaPool(t *testing.T, ctx context.Context, base *pgxpool.Pool, schema string) *pgxpool.Pool {
	t.Helper()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = base.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	cfg := base.Config().Copy()
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
