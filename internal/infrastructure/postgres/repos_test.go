package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/audit"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/state"
	"infra-guy/internal/domain/tenant"
	"infra-guy/internal/domain/tfcontext"
	"infra-guy/internal/domain/user"
	"infra-guy/internal/infrastructure/crypto"
)

func TestProjectRunAndStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	projects := NewProjectRepo(pool)
	runs := NewRunRepo(pool)
	states := NewStateRepo(pool)

	now := time.Unix(1_700_000_000, 0).UTC()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	projectID := project.ID("proj-" + suffix)
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New(projectID, "infra", repo, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetDefaultBranch("develop"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetWorkingDirectory("terraform"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetTerraformVersion("1.10.5"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetTagOverride(map[string]string{"env": "prod", "team": "core"}); err != nil {
		t.Fatal(err)
	}
	p.SetAutoApprove(true)
	p.SetStateToken("st-token")
	if err := p.ClaimActiveRun("run-" + suffix); err != nil {
		t.Fatal(err)
	}
	p.Lock()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, 'acme', $2, 'organization', now(), now())`, "ten-"+suffix, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := p.SetPlacement("ten-"+suffix, 99); err != nil {
		t.Fatal(err)
	}
	attachTestFolder(t, ctx, pool, p)
	if err := projects.Save(ctx, p); err != nil {
		t.Fatal(err)
	}

	other, err := project.New(project.ID("proj-other-"+suffix), "other", repo, now)
	if err != nil {
		t.Fatal(err)
	}
	placeTestProject(t, ctx, pool, other, "ten-other-"+suffix, 99)
	if err := projects.Save(ctx, other); err != nil {
		t.Fatal(err)
	}
	found, err := projects.FindByRepo(ctx, "ten-"+suffix, 99)
	if err != nil || len(found) != 1 || found[0].Snapshot().ID != projectID {
		t.Fatalf("FindByRepo leaked across tenants: %v %#v", err, found)
	}
	listed, err := projects.List(ctx, "ten-other-"+suffix)
	if err != nil || len(listed) != 1 || listed[0].Snapshot().ID != other.Snapshot().ID {
		t.Fatalf("List leaked across tenants: %v %#v", err, listed)
	}

	gotProject, err := projects.Get(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if gotProject == nil {
		t.Fatal("project not found")
	}
	wantP, gotP := p.Snapshot(), gotProject.Snapshot()
	if gotP.ID != wantP.ID || gotP.Name != wantP.Name || gotP.Repo != wantP.Repo ||
		gotP.DefaultBranch != wantP.DefaultBranch || gotP.WorkingDirectory != wantP.WorkingDirectory ||
		gotP.TerraformVersion != "1.10.5" || gotP.TerraformVersion != wantP.TerraformVersion ||
		gotP.AutoApprove != wantP.AutoApprove || gotP.Locked != wantP.Locked ||
		gotP.StateToken != wantP.StateToken || gotP.ActiveRunID != wantP.ActiveRunID ||
		gotP.TenantID != wantP.TenantID || gotP.GitHubRepoID != wantP.GitHubRepoID ||
		project.FormatTags(gotP.Tags) != "env:prod, team:core" ||
		!gotP.CreatedAt.Equal(wantP.CreatedAt) || !gotP.UpdatedAt.Equal(wantP.UpdatedAt) {
		t.Fatalf("project snapshot\nwant %#v\ngot  %#v", wantP, gotP)
	}

	prNumber := 42
	committedAt := now.Add(-30 * time.Minute)
	opened, err := run.Open(run.OpenInput{
		ID: run.ID("run-" + suffix), ProjectID: run.ProjectID(projectID),
		AutoApprove: true, PlanOnly: false, Trigger: run.TriggerPullRequest,
		SHA: "abc123", Branch: "feature", Message: "add vpc", Author: "bart",
		TriggeredBy: "github", PRNumber: &prNumber, PRTitle: "Add VPC",
		CommittedAt: &committedAt, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := runs.InsertIfAbsent(ctx, opened)
	if err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}
	again, err := run.Open(run.OpenInput{
		ID: run.ID("run-dup-" + suffix), ProjectID: run.ProjectID(projectID),
		AutoApprove: true, Trigger: run.TriggerPullRequest,
		SHA: "abc123", PRNumber: &prNumber, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	inserted, err = runs.InsertIfAbsent(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("second insert should return false")
	}

	gotRun, err := runs.Get(ctx, opened.Snapshot().ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun == nil {
		t.Fatal("run not found")
	}
	wantR, gotR := opened.Snapshot(), gotRun.Snapshot()
	if gotR.ID != wantR.ID || gotR.ProjectID != wantR.ProjectID || gotR.Trigger != wantR.Trigger ||
		gotR.Status != wantR.Status || gotR.SHA != wantR.SHA || gotR.Branch != wantR.Branch ||
		gotR.Message != wantR.Message || gotR.Author != wantR.Author || gotR.TriggeredBy != wantR.TriggeredBy ||
		gotR.PRNumber == nil || *gotR.PRNumber != prNumber || gotR.PRTitle != wantR.PRTitle ||
		gotR.PlanOnly != wantR.PlanOnly || gotR.AutoApprove != wantR.AutoApprove ||
		gotR.CommittedAt == nil || !gotR.CommittedAt.Equal(committedAt) ||
		!gotR.CreatedAt.Equal(wantR.CreatedAt) || len(gotR.Steps) != len(wantR.Steps) {
		t.Fatalf("run snapshot\nwant %#v\ngot  %#v", wantR, gotR)
	}
	for i := range wantR.Steps {
		if gotR.Steps[i].Name != wantR.Steps[i].Name || gotR.Steps[i].Position != wantR.Steps[i].Position ||
			gotR.Steps[i].Status != wantR.Steps[i].Status {
			t.Fatalf("step %d want %#v got %#v", i, wantR.Steps[i], gotR.Steps[i])
		}
	}

	opened.SetTerraformVersion("1.9.0")
	if err := opened.BeginStep(run.StepInit, now); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.CompleteStep(run.StepInit, run.StepResult{}, now); err != nil {
		t.Fatal(err)
	}
	if err := opened.BeginStep(run.StepPlan, now); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.CompleteStep(run.StepPlan, run.StepResult{
		PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.\n",
		PlanFile: []byte("tfplan-bytes"),
		PlanJSON: []byte(`{"format_version":"1.2"}`),
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := runs.Save(ctx, opened); err != nil {
		t.Fatal(err)
	}
	savedRun, err := runs.Get(ctx, opened.Snapshot().ID)
	if err != nil || savedRun == nil || savedRun.Snapshot().TerraformVersion != "1.9.0" || string(savedRun.Snapshot().PlanJSON) != `{"format_version":"1.2"}` {
		t.Fatalf("save/get: %v %#v", err, savedRun)
	}

	summaries, err := runs.ListSummariesByTenant(ctx, "ten-"+suffix)
	if err != nil || len(summaries) != 1 {
		t.Fatalf("ListSummariesByTenant: err=%v len=%d", err, len(summaries))
	}
	sumSnap := summaries[0].Snapshot()
	if sumSnap.ID != opened.Snapshot().ID || sumSnap.Add != 1 || len(sumSnap.Steps) != 3 ||
		sumSnap.CommittedAt == nil || !sumSnap.CommittedAt.Equal(committedAt) ||
		sumSnap.PlanText != "" || len(sumSnap.PlanFile) != 0 || len(sumSnap.PlanJSON) != 0 {
		t.Fatalf("unexpected summary snapshot: %#v", sumSnap)
	}

	st, err := states.Load(ctx, string(projectID))
	if err != nil {
		t.Fatal(err)
	}
	if st != nil {
		t.Fatalf("expected nil state before first save, got %+v", st)
	}
	st = state.New(string(projectID))
	body := []byte(`{"version":4,"serial":7}`)
	if err := st.Put(body, "", string(opened.Snapshot().ID), now); err != nil {
		t.Fatal(err)
	}
	if err := states.Save(ctx, st); err != nil {
		t.Fatal(err)
	}
	loaded, err := states.Load(ctx, string(projectID))
	if err != nil {
		t.Fatal(err)
	}
	snap := loaded.Snapshot()
	if string(snap.Data) != string(body) || snap.Serial == nil || *snap.Serial != 7 || !snap.UpdatedAt.Equal(now) {
		t.Fatalf("state %#v", snap)
	}
}

func TestInsertIfAbsentConcurrentSameKeyNilPR(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	projects := NewProjectRepo(pool)
	runs := NewRunRepo(pool)

	now := time.Unix(1_700_000_000, 0).UTC()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	projectID := project.ID("proj-conc-" + suffix)
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New(projectID, "infra", repo, now)
	if err != nil {
		t.Fatal(err)
	}
	placeTestProject(t, ctx, pool, p, "ten-conc-"+suffix, 1)
	if err := projects.Save(ctx, p); err != nil {
		t.Fatal(err)
	}

	const sha = "deadbeef"
	const n = 2
	var wg sync.WaitGroup
	var insertedCount atomic.Int32
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			opened, err := run.Open(run.OpenInput{
				ID:        run.ID(fmt.Sprintf("run-conc-%d-%s", i, suffix)),
				ProjectID: run.ProjectID(projectID),
				Trigger:   run.TriggerPush,
				SHA:       sha,
				Now:       now,
			})
			if err != nil {
				errCh <- err
				return
			}
			inserted, err := runs.InsertIfAbsent(ctx, opened)
			if err != nil {
				errCh <- err
				return
			}
			if inserted {
				insertedCount.Add(1)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := insertedCount.Load(); got != 1 {
		t.Fatalf("inserted count = %d, want 1", got)
	}

	var runRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM runs
		WHERE project_id = $1 AND commit_sha = $2 AND trigger = $3 AND pr_number IS NULL
			AND status IN ('queued', 'running')`,
		projectID, sha, run.TriggerPush,
	).Scan(&runRows); err != nil {
		t.Fatal(err)
	}
	if runRows != 1 {
		t.Fatalf("active run rows = %d, want 1", runRows)
	}
}

func newTestProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projects *ProjectRepo, prefix string) *project.Project {
	t.Helper()
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New(project.ID(fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())), "infra", repo, time.Unix(1_700_000_000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	placeTestProject(t, ctx, pool, p, "ten-"+string(p.Snapshot().ID), 1)
	if err := projects.Save(ctx, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectSettingsSaveKeepsActiveRunAndLock(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	projects := NewProjectRepo(pool)
	stale := newTestProject(t, ctx, pool, projects, "proj-settings")
	id := stale.Snapshot().ID

	if _, err := pool.Exec(ctx, `UPDATE projects SET active_run_id = 'run-live' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := projects.SetLocked(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	stale.SetAutoApprove(true)
	if err := projects.Save(ctx, stale); err != nil {
		t.Fatal(err)
	}
	got, err := projects.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if s := got.Snapshot(); s.ActiveRunID != "run-live" || !s.Locked || !s.AutoApprove {
		t.Fatalf("settings save clobbered columns: %#v", s)
	}

	if err := projects.ReleaseActiveRun(ctx, id, "run-other"); err != nil {
		t.Fatal(err)
	}
	if got, _ := projects.Get(ctx, id); got.Snapshot().ActiveRunID != "run-live" {
		t.Fatalf("mismatched release cleared %q", got.Snapshot().ActiveRunID)
	}
	if err := projects.ReleaseActiveRun(ctx, id, "run-live"); err != nil {
		t.Fatal(err)
	}
	if got, _ := projects.Get(ctx, id); got.Snapshot().ActiveRunID != "" {
		t.Fatalf("release left %q", got.Snapshot().ActiveRunID)
	}
}

func TestStateSaveAndImportPreservesChangedOutputs(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	projects := NewProjectRepo(pool)
	states := NewStateRepo(pool)
	id := string(newTestProject(t, ctx, pool, projects, "proj-state").Snapshot().ID)

	st := state.New(id)
	if err := st.Put([]byte(`{"serial":1,"outputs":{"vpc":{"value":"v1"}}}`), "", "run-0", time.Unix(1_700_000_000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if err := states.Save(ctx, st); err != nil {
		t.Fatal(err)
	}
	loaded, err := states.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Snapshot().ChangedOutputsSincePrevious()["vpc"] {
		t.Fatalf("expected vpc marked changed after first put: %#v", loaded.Snapshot())
	}

	if err := loaded.Import([]byte(`{"serial":2,"outputs":{"vpc":{"value":"v1"},"subnet":{"value":"s1"}}}`), time.Unix(1_700_000_001, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.Lock([]byte(`{"ID":"L1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := states.Save(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	loaded, err = states.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	changed := loaded.Snapshot().ChangedOutputsSincePrevious()
	if changed["vpc"] || !changed["subnet"] || loaded.Snapshot().LockID != "L1" {
		t.Fatalf("loaded %#v (changed=%+v)", loaded.Snapshot(), changed)
	}
}

func TestRunSaveActiveConflictIsSentinel(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	projects := NewProjectRepo(pool)
	runs := NewRunRepo(pool)
	pid := run.ProjectID(newTestProject(t, ctx, pool, projects, "proj-conflict").Snapshot().ID)
	now := time.Unix(1_700_000_000, 0).UTC()

	waiting := run.Restore(run.Snapshot{
		ID: run.ID(string(pid) + "-a"), ProjectID: pid, Trigger: run.TriggerPush, Status: run.StatusNeedsApproval,
		SHA: "abc", CreatedAt: now,
	})
	if ok, err := runs.InsertIfAbsent(ctx, waiting); err != nil || !ok {
		t.Fatalf("insert waiting %v %v", ok, err)
	}
	queued, err := run.Open(run.OpenInput{ID: run.ID(string(pid) + "-b"), ProjectID: pid, Trigger: run.TriggerPush, SHA: "abc", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := runs.InsertIfAbsent(ctx, queued); err != nil || !ok {
		t.Fatalf("insert queued %v %v", ok, err)
	}
	if _, err := waiting.Confirm("operator", now); err != nil {
		t.Fatal(err)
	}
	if err := runs.Save(ctx, waiting); !errors.Is(err, run.ErrActiveConflict) {
		t.Fatalf("save conflict: %v", err)
	}
}

func openTestPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// placeTestProject inserts a tenant row and puts p on it.
func placeTestProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, p *project.Project, tenantID string, repoID int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, 'acme', $2, 'organization', now(), now()) ON CONFLICT (id) DO NOTHING`, tenantID, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := p.SetPlacement(tenantID, repoID); err != nil {
		t.Fatal(err)
	}
	attachTestFolder(t, ctx, pool, p)
}

func attachTestFolder(t *testing.T, ctx context.Context, pool *pgxpool.Pool, p *project.Project) {
	t.Helper()
	tenantID := p.Snapshot().TenantID
	root, err := NewFolderRepo(pool).EnsureRoot(ctx, tenantID, "root-"+tenantID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	p.SetFolderID(root.ID)
}

func TestIdentityStore(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := NewIdentityStore(pool)
	now := time.Unix(1_700_000_000, 0).UTC()
	n := time.Now().UnixNano()
	suffix := fmt.Sprintf("%d", n)

	u, err := store.UpsertUser(ctx, user.User{ID: "usr-" + suffix, Login: "ada"}, user.GitHubIdentity("usr-"+suffix, n), now)
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.UpsertUser(ctx, user.User{ID: "usr-other-" + suffix, Login: "ada2", DisplayName: "Ada"}, user.GitHubIdentity("usr-other-"+suffix, n), now)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != u.ID || again.Login != "ada2" || again.DisplayName != "Ada" {
		t.Fatalf("user upsert: %#v then %#v", u, again)
	}
	if has, err := store.HasIdentity(ctx, u.ID, "github", "https://github.com", fmt.Sprintf("%d", n)); err != nil || !has {
		t.Fatalf("HasIdentity true: has=%v err=%v", has, err)
	}
	if has, err := store.HasIdentity(ctx, u.ID, "github", "https://github.com", "wrong"); err != nil || has {
		t.Fatalf("HasIdentity false: has=%v err=%v", has, err)
	}

	ten, err := store.UpsertTenant(ctx, tenant.Tenant{ID: "ten-" + suffix, Name: "acme", AccountType: "organization", GitHubAccountID: n}, now)
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := store.UpsertTenant(ctx, tenant.Tenant{ID: "ten-x-" + suffix, Name: "acme2", AccountType: "organization", GitHubAccountID: n}, now)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != ten.ID || renamed.Name != "acme2" {
		t.Fatalf("tenant upsert: %#v then %#v", ten, renamed)
	}

	first := tenant.Installation{ID: "ins-1-" + suffix, TenantID: ten.ID, AccountLogin: "acme", AccountType: "organization", GitHubInstallationID: n}
	second := tenant.Installation{ID: "ins-2-" + suffix, TenantID: ten.ID, AccountLogin: "acme", AccountType: "organization", GitHubInstallationID: n + 1}
	if err := store.UpsertInstallation(ctx, first, now); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertInstallation(ctx, second, now); err != nil {
		t.Fatal(err)
	}
	active, err := store.ActiveInstallation(ctx, ten.ID)
	if err != nil || active == nil || active.GitHubInstallationID != n+1 || !active.Active {
		t.Fatalf("active: %v %#v", err, active)
	}
	old, err := store.InstallationByGitHubID(ctx, n)
	if err != nil || old == nil || old.Active {
		t.Fatalf("old installation: %v %#v", err, old)
	}
	if listed, err := store.ListActiveInstallations(ctx); err != nil || !containsInstallation(listed, n+1) || containsInstallation(listed, n) {
		t.Fatalf("list active: %v %#v", err, listed)
	}

	if err := store.UpsertMembership(ctx, tenant.Membership{TenantID: ten.ID, UserID: u.ID, Role: "member"}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMembership(ctx, tenant.Membership{TenantID: ten.ID, UserID: u.ID, Role: "admin"}, now); err != nil {
		t.Fatal(err)
	}
	mem, err := store.GetMembership(ctx, ten.ID, u.ID)
	if err != nil || mem == nil || mem.Role != "admin" || mem.TenantName != "acme" {
		t.Fatalf("GetMembership: %v %#v", err, mem)
	}
	mems, err := store.ListUserMemberships(ctx, u.ID)
	if err != nil || len(mems) != 1 || mems[0].TenantID != ten.ID || mems[0].Role != "admin" || mems[0].TenantName != "acme" {
		t.Fatalf("ListUserMemberships: %v %#v", err, mems)
	}

	if err := store.DeactivateInstallation(ctx, n+1, now); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ActiveInstallation(ctx, ten.ID); err != nil || got != nil {
		t.Fatalf("after deactivate: %v %#v", err, got)
	}
	if got, err := store.GetMembership(ctx, ten.ID, u.ID); err != nil || got != nil {
		t.Fatalf("GetMembership after deactivate: %v %#v", err, got)
	}
	if err := store.DeleteMembership(ctx, ten.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := store.InstallationByGitHubID(ctx, n+99); err != nil || got != nil {
		t.Fatalf("missing installation: %v %#v", err, got)
	}

	sess := Session{ID: "sess-" + suffix, UserID: u.ID, Login: "ada2", ExpiresAt: now.Add(time.Hour)}
	if err := store.SaveSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSession(ctx, sess.ID)
	if err != nil || got == nil || got.TenantID != "" || !got.ExpiresAt.Equal(sess.ExpiresAt) {
		t.Fatalf("session: %v %#v", err, got)
	}
	sess.TenantID, sess.Role = ten.ID, "admin"
	if err := store.SaveSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetSession(ctx, sess.ID); err != nil || got == nil || got.TenantID != ten.ID || got.Role != "admin" {
		t.Fatalf("session update: %v %#v", err, got)
	}
	if got, err := store.GetSession(ctx, "missing-"+suffix); err != nil || got != nil {
		t.Fatalf("missing session: %v %#v", err, got)
	}

	if err := store.SaveOAuthState(ctx, "st-"+suffix, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if exp, ok, err := store.ConsumeOAuthState(ctx, "st-"+suffix); err != nil || !ok || !exp.Equal(now.Add(time.Minute)) {
		t.Fatalf("consume: exp=%v ok=%v err=%v", exp, ok, err)
	}
	if _, ok, err := store.ConsumeOAuthState(ctx, "st-"+suffix); err != nil || ok {
		t.Fatalf("second consume: %v %v", ok, err)
	}
}

func containsInstallation(list []tenant.Installation, githubID int64) bool {
	for _, in := range list {
		if in.GitHubInstallationID == githubID {
			return true
		}
	}
	return false
}

func TestContextRepoCRUDAndEncryption(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	sealer, err := crypto.NewSealer([]byte("test-encryption-key"))
	if err != nil {
		t.Fatal(err)
	}
	repo := NewContextRepo(pool)
	projects := NewProjectRepo(pool)

	now := time.Unix(1_700_000_000, 0).UTC()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantID := "ten-ctx-" + suffix
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, 'acme', $2, 'organization', now(), now())`, tenantID, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	c1, err := tfcontext.New(tfcontext.ID("ctx-1-"+suffix), tenantID, "base", now)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := tfcontext.New(tfcontext.ID("ctx-2-"+suffix), tenantID, "override", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, c1); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, c2); err != nil {
		t.Fatal(err)
	}

	ePlain, _ := tfcontext.NewEntry("e1-"+suffix, tfcontext.KindPlainEnv, "TF_VAR_env", "staging", now)
	eSec, _ := tfcontext.NewEntry("e2-"+suffix, tfcontext.KindSecretEnv, "AWS_SECRET_ACCESS_KEY", "", now)
	eSec.Ciphertext, err = sealer.Seal([]byte("s3cr3t"))
	if err != nil {
		t.Fatal(err)
	}
	eFile, _ := tfcontext.NewEntry("e3-"+suffix, tfcontext.KindSecretFile, "~/.kube/config", "", now)
	eFile.Ciphertext, err = sealer.Seal([]byte("kube-yaml-1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []tfcontext.Entry{ePlain, eSec, eFile} {
		if err := repo.UpsertEntry(ctx, c1.Snapshot().ID, e); err != nil {
			t.Fatal(err)
		}
	}
	eOverride, _ := tfcontext.NewEntry("e4-"+suffix, tfcontext.KindPlainEnv, "TF_VAR_env", "production", now)
	if err := repo.UpsertEntry(ctx, c2.Snapshot().ID, eOverride); err != nil {
		t.Fatal(err)
	}

	// Verify secret is not stored in value_text in raw SQL
	var rawText string
	var rawCipher []byte
	if err := pool.QueryRow(ctx, `SELECT value_text, value_ciphertext FROM context_entries WHERE id = $1`, eSec.ID).Scan(&rawText, &rawCipher); err != nil {
		t.Fatal(err)
	}
	if rawText != "" || len(rawCipher) == 0 {
		t.Fatalf("secret stored in plaintext or missing ciphertext: text=%q cipherLen=%d", rawText, len(rawCipher))
	}

	// Get returns empty Value and stored Ciphertext for secret entries
	masked, err := repo.Get(ctx, c1.Snapshot().ID)
	if err != nil || masked == nil {
		t.Fatalf("get masked: %v", err)
	}
	for _, e := range masked.Snapshot().Entries {
		if e.Secret && e.Value != "" {
			t.Fatalf("expected masked secret to have empty value, got %q", e.Value)
		}
		if e.Secret && len(e.Ciphertext) == 0 {
			t.Fatalf("expected secret %s to have ciphertext", e.Key)
		}
		if !e.Secret && e.Value != "staging" {
			t.Fatalf("plain value = %q, want staging", e.Value)
		}
	}

	// Attach contexts in order [c1, c2] to a project and resolve decrypted
	srcRepo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New(project.ID("proj-ctx-"+suffix), "infra", srcRepo, now)
	_ = p.SetPlacement(tenantID, 100)
	attachTestFolder(t, ctx, pool, p)
	p.SetStateToken("tok")
	if err := projects.Save(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetProjectContexts(ctx, p.Snapshot().ID, []tfcontext.ID{c1.Snapshot().ID, c2.Snapshot().ID}); err != nil {
		t.Fatal(err)
	}
	attached, err := repo.ListByProject(ctx, p.Snapshot().ID)
	if err != nil || len(attached) != 2 {
		t.Fatalf("ListByProject: %v len=%d", err, len(attached))
	}
	for _, c := range attached {
		entries := c.Snapshot().Entries
		for i := range entries {
			if entries[i].Secret && len(entries[i].Ciphertext) > 0 {
				pt, err := sealer.Open(entries[i].Ciphertext)
				if err != nil {
					t.Fatal(err)
				}
				entries[i].Value = string(pt)
			}
		}
		c.SetEntries(entries)
	}
	env, files := tfcontext.Resolve(attached)
	if env["TF_VAR_env"] != "production" || env["AWS_SECRET_ACCESS_KEY"] != "s3cr3t" || string(files[".kube/config"]) != "kube-yaml-1" {
		t.Fatalf("resolved env=%v files=%v", env, files)
	}

	byTenant, err := repo.ListByTenant(ctx, tenantID)
	if err != nil || len(byTenant) != 2 {
		t.Fatalf("ListByTenant: %v len=%d", err, len(byTenant))
	}
	if err := repo.DeleteEntry(ctx, c1.Snapshot().ID, ePlain.ID); err != nil {
		t.Fatal(err)
	}
	afterEntryDel, err := repo.Get(ctx, c1.Snapshot().ID)
	if err != nil || len(afterEntryDel.Snapshot().Entries) != 2 {
		t.Fatalf("after DeleteEntry: %v len=%d", err, len(afterEntryDel.Snapshot().Entries))
	}
	if err := repo.Delete(ctx, c2.Snapshot().ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := repo.Get(ctx, c2.Snapshot().ID)
	if err != nil || deleted != nil {
		t.Fatalf("after Delete: err=%v got=%#v", err, deleted)
	}
}

func TestAuditRepoAppendListAndChainVerification(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	repo := NewAuditRepo(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantID := "ten-audit-" + suffix
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, 'acme', $2, 'organization', now(), now())`, tenantID, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	s1, err := repo.Append(ctx, audit.Snapshot{
		ID:           audit.ID("aud-1-" + suffix),
		TenantID:     tenantID,
		ActorID:      "u-1",
		ActorLogin:   "alice",
		ActorRole:    "admin",
		Action:       "workspace.created",
		ResourceType: "workspace",
		ResourceID:   "p-1",
		ResourceName: "prod-vpc",
		Summary:      "Created workspace prod-vpc",
		Metadata:     map[string]any{"provider": "opentofu"},
		CreatedAt:    now,
	})
	if err != nil {
		t.Fatalf("append 1: %v", err)
	}
	s2, err := repo.Append(ctx, audit.Snapshot{
		ID:           audit.ID("aud-2-" + suffix),
		TenantID:     tenantID,
		ActorID:      "u-2",
		ActorLogin:   "bob",
		ActorRole:    "member",
		Action:       "run.confirmed",
		ResourceType: "run",
		ResourceID:   "r-1",
		ResourceName: "prod-vpc",
		Summary:      "Confirmed apply for run r-1 on workspace prod-vpc",
		Metadata:     map[string]any{"status": "applying"},
		CreatedAt:    now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if s2.PrevHash != s1.EntryHash {
		t.Fatalf("expected s2.PrevHash (%q) == s1.EntryHash (%q)", s2.PrevHash, s1.EntryHash)
	}

	entries, err := repo.ListByTenant(ctx, tenantID, audit.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if !audit.VerifyChain(entries, true) {
		t.Fatalf("expected hash chain to verify after postgres round-trip")
	}
}

func TestSecurityEnvelopeEncryptionTokenHashingAndKeyRotation(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	sealerK1, err := crypto.ParseKeyring("k1:secret-k1", "k1", nil)
	if err != nil {
		t.Fatal(err)
	}

	projects := NewProjectRepo(pool).WithSealer(sealerK1)
	states := NewStateRepo(pool).WithSealer(sealerK1)
	runs := NewRunRepo(pool).WithSealer(sealerK1)
	identStore := NewIdentityStore(pool)

	p := newTestProject(t, ctx, pool, projects, "proj-sec")
	p.SetStateToken("tok-secret-123")
	if err := projects.Save(ctx, p); err != nil {
		t.Fatal(err)
	}
	pid := p.Snapshot().ID
	origToken := p.Snapshot().StateToken

	var rawToken, tokenHash string
	var tokenCiphertext []byte
	if err := pool.QueryRow(ctx, `SELECT state_token, state_token_hash, state_token_ciphertext FROM projects WHERE id = $1`, pid).Scan(&rawToken, &tokenHash, &tokenCiphertext); err != nil {
		t.Fatal(err)
	}
	if rawToken != "" {
		t.Fatalf("expected plaintext state_token column to be empty, got %q", rawToken)
	}
	if !crypto.VerifyTokenHash(origToken, tokenHash) {
		t.Fatalf("expected state_token_hash %q to verify original token", tokenHash)
	}
	if kid, ok := crypto.EnvelopeKeyID(tokenCiphertext); !ok || kid != "k1" {
		t.Fatalf("expected envelope ciphertext with k1, got %q", string(tokenCiphertext))
	}
	gotProj, err := projects.Get(ctx, pid)
	if err != nil || gotProj == nil || gotProj.Snapshot().StateToken != origToken {
		t.Fatalf("decrypted project StateToken mismatch: %v", err)
	}

	st := state.New(string(pid))
	stateBytes := []byte(`{"serial":1,"outputs":{"secret_db_password":{"value":"hunter2","sensitive":true}}}`)
	if err := st.Put(stateBytes, "", "run-sec", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := states.Save(ctx, st); err != nil {
		t.Fatal(err)
	}
	var rawStateData []byte
	if err := pool.QueryRow(ctx, `SELECT data FROM states WHERE project_id = $1`, pid).Scan(&rawStateData); err != nil {
		t.Fatal(err)
	}
	if kid, ok := crypto.EnvelopeKeyID(rawStateData); !ok || kid != "k1" {
		t.Fatalf("expected encrypted states.data with k1")
	}
	loadedState, err := states.Load(ctx, string(pid))
	if err != nil || string(loadedState.Snapshot().Data) != string(stateBytes) {
		t.Fatalf("loaded state mismatch: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	r, err := run.Open(run.OpenInput{
		ID:        run.ID("run-sec-" + string(pid)),
		ProjectID: run.ProjectID(pid),
		Trigger:   run.TriggerPush,
		SHA:       "1234567",
		Now:       now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := runs.InsertIfAbsent(ctx, r); err != nil || !ok {
		t.Fatalf("InsertIfAbsent: %v %v", ok, err)
	}
	_ = r.BeginStep(run.StepInit, now)
	_, _ = r.CompleteStep(run.StepInit, run.StepResult{ExitCode: 0}, now)
	_ = r.BeginStep(run.StepPlan, now)
	planFile := []byte("tfplan-binary-secret-contents")
	planJSON := []byte(`{"format_version":"1.2","variables":{"db_pass":{"value":"supersecret"}}}`)
	_, _ = r.CompleteStep(run.StepPlan, run.StepResult{
		ExitCode: 0,
		PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.",
		PlanFile: planFile,
		PlanJSON: planJSON,
	}, now)
	if err := runs.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	var rawPlanFile, rawPlanJSON []byte
	if err := pool.QueryRow(ctx, `SELECT plan_file, plan_json FROM runs WHERE id = $1`, r.Snapshot().ID).Scan(&rawPlanFile, &rawPlanJSON); err != nil {
		t.Fatal(err)
	}
	if !crypto.IsEnvelopeCiphertext(rawPlanFile) || !crypto.IsEnvelopeCiphertext(rawPlanJSON) {
		t.Fatalf("expected encrypted plan_file and plan_json in DB")
	}
	loadedRun, err := runs.Get(ctx, r.Snapshot().ID)
	if err != nil || string(loadedRun.Snapshot().PlanFile) != string(planFile) || string(loadedRun.Snapshot().PlanJSON) != string(planJSON) {
		t.Fatalf("loaded run plan mismatch: %v", err)
	}

	// Verify session token hashing in IdentityStore
	uid := "usr-sec-" + string(pid)
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, login, display_name, created_at, updated_at) VALUES ($1, 'secuser', 'Sec User', now(), now())`, uid); err != nil {
		t.Fatal(err)
	}
	rawSessID := "sess-raw-token-" + string(pid)
	if err := identStore.SaveSession(ctx, Session{
		ID:        rawSessID,
		UserID:    uid,
		TenantID:  p.Snapshot().TenantID,
		ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var storedSessID string
	if err := pool.QueryRow(ctx, `SELECT id FROM sessions WHERE user_id = $1`, uid).Scan(&storedSessID); err != nil {
		t.Fatal(err)
	}
	if storedSessID != crypto.HashToken(rawSessID) {
		t.Fatalf("expected session id in DB to be hashed %q, got %q", crypto.HashToken(rawSessID), storedSessID)
	}
	gotSess, err := identStore.GetSession(ctx, rawSessID)
	if err != nil || gotSess == nil || gotSess.ID != rawSessID {
		t.Fatalf("GetSession by raw token failed: %v %#v", err, gotSess)
	}

	// Verify RotateAndSealAll rotates from k1 to k2
	sealerK2, err := crypto.ParseKeyring("k2:secret-k2,k1:secret-k1,default:test-encryption-key", "k2", nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := RotateAndSealAll(ctx, pool, sealerK2)
	if err != nil {
		t.Fatalf("RotateAndSealAll: %v", err)
	}
	if stats.ProjectTokens == 0 || stats.States == 0 || stats.RunPlanArtifacts == 0 {
		t.Fatalf("expected non-zero rotation counts, got %+v", stats)
	}
	if err := pool.QueryRow(ctx, `SELECT data FROM states WHERE project_id = $1`, pid).Scan(&rawStateData); err != nil {
		t.Fatal(err)
	}
	if kid, ok := crypto.EnvelopeKeyID(rawStateData); !ok || kid != "k2" {
		t.Fatalf("expected rotated state key ID k2, got %q", kid)
	}
}
