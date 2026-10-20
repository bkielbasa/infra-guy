package postgres

import (
	"context"
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
	p.SetAutoApprove(true)
	p.SetGitHubToken("gh-token")
	p.RotateWebhookSecret("wh-secret")
	p.SetStateToken("st-token")
	p.Lock()
	if err := p.ClaimActiveRun("run-" + suffix); err != nil {
		t.Fatal(err)
	}
	if err := projects.Save(ctx, p); err != nil {
		t.Fatal(err)
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
		gotP.AutoApprove != wantP.AutoApprove || gotP.Locked != wantP.Locked ||
		gotP.GitHubToken != wantP.GitHubToken || gotP.WebhookSecret != wantP.WebhookSecret ||
		gotP.StateToken != wantP.StateToken || gotP.ActiveRunID != wantP.ActiveRunID ||
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
	if err := runs.Save(ctx, opened); err != nil {
		t.Fatal(err)
	}
	savedRun, err := runs.Get(ctx, opened.Snapshot().ID)
	if err != nil || savedRun == nil || savedRun.Snapshot().TerraformVersion != "1.9.0" {
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
				ID: run.ID(fmt.Sprintf("run-conc-%d-%s", i, suffix)),
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
