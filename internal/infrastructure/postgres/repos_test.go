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
	p.SetAutoApprove(true)
	p.SetStateToken("st-token")
	p.Lock()
	if err := p.ClaimActiveRun("run-" + suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, 'acme', $2, 'organization', now(), now())`, "ten-"+suffix, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := p.SetPlacement("ten-"+suffix, 99); err != nil {
		t.Fatal(err)
	}
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
		!gotP.CreatedAt.Equal(wantP.CreatedAt) || !gotP.UpdatedAt.Equal(wantP.UpdatedAt) {
		t.Fatalf("project snapshot\nwant %#v\ngot  %#v", wantP, gotP)
	}

	prNumber := 42
	opened, err := run.Open(run.OpenInput{
		ID: run.ID("run-" + suffix), ProjectID: run.ProjectID(projectID),
		AutoApprove: true, PlanOnly: false, Trigger: run.TriggerPullRequest,
		SHA: "abc123", Branch: "feature", Message: "add vpc", Author: "bart",
		TriggeredBy: "github", PRNumber: &prNumber, PRTitle: "Add VPC", Now: now,
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

	st, err := states.Load(ctx, string(projectID))
	if err != nil {
		t.Fatal(err)
	}
	if st.Snapshot().ProjectID != state.New(string(projectID)).Snapshot().ProjectID || !st.Empty() {
		t.Fatal("expected empty state before put")
	}
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
	if string(snap.Data) != string(body) || snap.Serial == nil || *snap.Serial != 7 {
		t.Fatalf("state %#v", snap)
	}
	if len(snap.History) != 1 {
		t.Fatalf("history rows: %d %#v", len(snap.History), snap.History)
	}
	h := snap.History[0]
	if h.RunID != string(opened.Snapshot().ID) || string(h.Data) != string(body) ||
		h.Serial == nil || *h.Serial != 7 || !h.CreatedAt.Equal(now) {
		t.Fatalf("history %#v", h)
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

func TestStateHistoryOnlyGrows(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	projects := NewProjectRepo(pool)
	states := NewStateRepo(pool)
	id := string(newTestProject(t, ctx, pool, projects, "proj-history").Snapshot().ID)

	for i, body := range []string{`{"serial":1}`, `{"serial":2}`} {
		st, err := states.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Put([]byte(body), "", fmt.Sprintf("run-%d", i), time.Unix(int64(1_700_000_000+i), 0).UTC()); err != nil {
			t.Fatal(err)
		}
		if err := states.Save(ctx, st); err != nil {
			t.Fatal(err)
		}
	}
	var firstID int64
	if err := pool.QueryRow(ctx, `SELECT min(id) FROM state_history WHERE project_id = $1`, id).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	st, err := states.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Lock([]byte(`{"ID":"L1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := states.Save(ctx, st); err != nil {
		t.Fatal(err)
	}
	var rows int
	var minID int64
	if err := pool.QueryRow(ctx, `SELECT count(*), min(id) FROM state_history WHERE project_id = $1`, id).Scan(&rows, &minID); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || minID != firstID {
		t.Fatalf("history rows %d first id %d (was %d)", rows, minID, firstID)
	}
	loaded, err := states.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if h := loaded.Snapshot().History; len(h) != 2 || h[0].RunID != "run-0" || h[1].RunID != "run-1" || loaded.Snapshot().LockID != "L1" {
		t.Fatalf("loaded %#v", loaded.Snapshot())
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

	u, err := store.UpsertUser(ctx, user.User{ID: "usr-" + suffix, GitHubUserID: n, Login: "ada"}, now)
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.UpsertUser(ctx, user.User{ID: "usr-other-" + suffix, GitHubUserID: n, Login: "ada2", DisplayName: "Ada"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != u.ID || again.Login != "ada2" || again.DisplayName != "Ada" {
		t.Fatalf("user upsert: %#v then %#v", u, again)
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
	if err := store.DeactivateInstallation(ctx, n+1, now); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ActiveInstallation(ctx, ten.ID); err != nil || got != nil {
		t.Fatalf("after deactivate: %v %#v", err, got)
	}
	if got, err := store.InstallationByGitHubID(ctx, n+99); err != nil || got != nil {
		t.Fatalf("missing installation: %v %#v", err, got)
	}

	sess := Session{ID: "sess-" + suffix, UserID: u.ID, Login: "ada2", AccessToken: "tok", ExpiresAt: now.Add(time.Hour)}
	if err := store.SaveSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSession(ctx, sess.ID)
	if err != nil || got == nil || got.TenantID != "" || got.AccessToken != "tok" || !got.ExpiresAt.Equal(sess.ExpiresAt) {
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
	if ok, err := store.ConsumeOAuthState(ctx, "st-"+suffix, now); err != nil || !ok {
		t.Fatalf("consume: %v %v", ok, err)
	}
	if ok, err := store.ConsumeOAuthState(ctx, "st-"+suffix, now); err != nil || ok {
		t.Fatalf("second consume: %v %v", ok, err)
	}
	if err := store.SaveOAuthState(ctx, "old-"+suffix, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.ConsumeOAuthState(ctx, "old-"+suffix, now); err != nil || ok {
		t.Fatalf("expired consume: %v %v", ok, err)
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
	repo := NewContextRepo(pool, sealer)
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
	eSec, _ := tfcontext.NewEntry("e2-"+suffix, tfcontext.KindSecretEnv, "AWS_SECRET_ACCESS_KEY", "s3cr3t", now)
	eFile, _ := tfcontext.NewEntry("e3-"+suffix, tfcontext.KindSecretFile, "~/.kube/config", "kube-yaml-1", now)
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

	// Get with decryptSecrets=false masks secret values
	masked, err := repo.Get(ctx, c1.Snapshot().ID, false)
	if err != nil || masked == nil {
		t.Fatalf("get masked: %v", err)
	}
	for _, e := range masked.Snapshot().Entries {
		if e.Secret && e.Value != "" {
			t.Fatalf("expected masked secret to have empty value, got %q", e.Value)
		}
		if !e.Secret && e.Value != "staging" {
			t.Fatalf("plain value = %q, want staging", e.Value)
		}
	}

	// Attach contexts in order [c1, c2] to a project and resolve decrypted
	srcRepo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New(project.ID("proj-ctx-"+suffix), "infra", srcRepo, now)
	_ = p.SetPlacement(tenantID, 100)
	p.SetStateToken("tok")
	if err := projects.Save(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetProjectContexts(ctx, p.Snapshot().ID, []tfcontext.ID{c1.Snapshot().ID, c2.Snapshot().ID}); err != nil {
		t.Fatal(err)
	}
	attached, err := repo.ListByProject(ctx, p.Snapshot().ID, true)
	if err != nil || len(attached) != 2 {
		t.Fatalf("ListByProject: %v len=%d", err, len(attached))
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
	afterEntryDel, err := repo.Get(ctx, c1.Snapshot().ID, false)
	if err != nil || len(afterEntryDel.Snapshot().Entries) != 2 {
		t.Fatalf("after DeleteEntry: %v len=%d", err, len(afterEntryDel.Snapshot().Entries))
	}
	if err := repo.Delete(ctx, c2.Snapshot().ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := repo.Get(ctx, c2.Snapshot().ID, false)
	if err != nil || deleted != nil {
		t.Fatalf("after Delete: err=%v got=%#v", err, deleted)
	}
}
