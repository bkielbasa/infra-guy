package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/application/worker"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
)

func TestQueueClaimBusyDelayReap(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	clearJobs(t, ctx, pool)

	projects := NewProjectRepo(pool)
	runs := NewRunRepo(pool)
	q := NewQueue(pool)
	now := time.Unix(1_700_000_000, 0).UTC()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	projectID := project.ID("proj-q-" + suffix)
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New(projectID, "infra", repo, now)
	if err != nil {
		t.Fatal(err)
	}
	placeTestProject(t, ctx, pool, p, "ten-"+string(projectID), 1)
	if err := projects.Save(ctx, p); err != nil {
		t.Fatal(err)
	}

	run1ID := run.ID("run-q1-" + suffix)
	run1, err := run.Open(run.OpenInput{
		ID: run1ID, ProjectID: run.ProjectID(projectID),
		Trigger: run.TriggerPush, SHA: "sha-q1-" + suffix, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := runs.InsertIfAbsent(ctx, run1)
	if err != nil || !inserted {
		t.Fatalf("run1 insert: inserted=%v err=%v", inserted, err)
	}

	if err := q.Enqueue(ctx, run1ID, run.StepInit); err != nil {
		t.Fatal(err)
	}

	job, err := q.Claim(ctx, "worker-1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Fatal("expected claimed init job")
	}
	if job.RunID != run1ID || job.Step != run.StepInit || job.Attempts != 1 {
		t.Fatalf("claimed job %#v", job)
	}

	gotProject, err := projects.Get(ctx, projectID)
	if err != nil || gotProject == nil {
		t.Fatalf("get project: %v", err)
	}
	if gotProject.Snapshot().ActiveRunID != string(run1ID) {
		t.Fatalf("active_run_id = %q, want %q", gotProject.Snapshot().ActiveRunID, run1ID)
	}

	run2ID := run.ID("run-q2-" + suffix)
	run2, err := run.Open(run.OpenInput{
		ID: run2ID, ProjectID: run.ProjectID(projectID),
		Trigger: run.TriggerPush, SHA: "sha-q2-" + suffix, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	inserted, err = runs.InsertIfAbsent(ctx, run2)
	if err != nil || !inserted {
		t.Fatalf("run2 insert: inserted=%v err=%v", inserted, err)
	}
	if err := q.Enqueue(ctx, run2ID, run.StepInit); err != nil {
		t.Fatal(err)
	}

	busy, err := q.Claim(ctx, "worker-1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if busy != nil {
		t.Fatalf("expected nil job while project is busy, got %#v", busy)
	}

	var delayedID int64
	var availableAt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT id, available_at FROM jobs WHERE run_id = $1 AND step_name = $2`,
		run2ID, run.StepInit,
	).Scan(&delayedID, &availableAt); err != nil {
		t.Fatal(err)
	}
	if remain := time.Until(availableAt); remain < 4*time.Second {
		t.Fatalf("available_at remaining %s, want at least 4s", remain)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET locked_at = now() - interval '3 minutes' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}

	requeue, failed, err := q.Reap(ctx, 2*time.Minute, worker.ReapPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if !containsJob(requeue, job.ID) || containsJob(failed, job.ID) {
		t.Fatalf("first reap requeue=%#v failed=%#v, want init job %d in requeue", requeue, failed, job.ID)
	}
	if len(requeue) != 1 || requeue[0].Attempts != 1 || requeue[0].Step != run.StepInit {
		t.Fatalf("requeue %#v", requeue)
	}

	claimedAgain, err := q.Claim(ctx, "worker-1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if claimedAgain == nil || claimedAgain.ID != job.ID || claimedAgain.Attempts != 2 {
		t.Fatalf("reclaim %#v, want id=%d attempts=2", claimedAgain, job.ID)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET locked_at = now() - interval '3 minutes' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}

	requeue, failed, err = q.Reap(ctx, 2*time.Minute, worker.ReapPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if containsJob(requeue, job.ID) || !containsJob(failed, job.ID) {
		t.Fatalf("second reap requeue=%#v failed=%#v, want init job %d in failed", requeue, failed, job.ID)
	}
	if lastErr := jobLastError(t, ctx, pool, job.ID); lastErr != "worker lost the step" {
		t.Fatalf("init last_error = %q", lastErr)
	}

	run3ID := run.ID("run-q3-" + suffix)
	run3, err := run.Open(run.OpenInput{
		ID: run3ID, ProjectID: run.ProjectID(projectID),
		Trigger: run.TriggerPush, SHA: "sha-q3-" + suffix, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	inserted, err = runs.InsertIfAbsent(ctx, run3)
	if err != nil || !inserted {
		t.Fatalf("run3 insert: inserted=%v err=%v", inserted, err)
	}
	if err := q.Enqueue(ctx, run3ID, run.StepApply); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET status = 'running', attempts = 1,
			locked_at = now() - interval '3 minutes', locked_by = 'worker-1'
		WHERE run_id = $1 AND step_name = $2`,
		run3ID, run.StepApply,
	); err != nil {
		t.Fatal(err)
	}

	var applyID int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM jobs WHERE run_id = $1 AND step_name = $2`,
		run3ID, run.StepApply,
	).Scan(&applyID); err != nil {
		t.Fatal(err)
	}

	requeue, failed, err = q.Reap(ctx, 2*time.Minute, worker.ReapPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if containsJob(requeue, applyID) || !containsJob(failed, applyID) {
		t.Fatalf("apply reap requeue=%#v failed=%#v, want apply job %d in failed", requeue, failed, applyID)
	}
	if lastErr := jobLastError(t, ctx, pool, applyID); lastErr != "worker lost the apply; check state before starting another run" {
		t.Fatalf("apply last_error = %q", lastErr)
	}
}

func TestQueueHeartbeatAndComplete(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	clearJobs(t, ctx, pool)

	projects := NewProjectRepo(pool)
	runs := NewRunRepo(pool)
	q := NewQueue(pool)
	now := time.Unix(1_700_000_000, 0).UTC()
	suffix := fmt.Sprintf("hb-%d", time.Now().UnixNano())

	projectID := project.ID("proj-" + suffix)
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New(projectID, "infra", repo, now)
	if err != nil {
		t.Fatal(err)
	}
	placeTestProject(t, ctx, pool, p, "ten-"+string(projectID), 1)
	if err := projects.Save(ctx, p); err != nil {
		t.Fatal(err)
	}

	runID := run.ID("run-" + suffix)
	opened, err := run.Open(run.OpenInput{
		ID: runID, ProjectID: run.ProjectID(projectID),
		Trigger: run.TriggerManual, SHA: "sha-" + suffix, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := runs.InsertIfAbsent(ctx, opened)
	if err != nil || !inserted {
		t.Fatalf("insert: inserted=%v err=%v", inserted, err)
	}

	if err := q.Enqueue(ctx, runID, run.StepPlan); err != nil {
		t.Fatal(err)
	}
	var pendingID int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM jobs WHERE run_id = $1 AND step_name = $2`,
		runID, run.StepPlan,
	).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	if err := q.Heartbeat(ctx, pendingID); err != nil {
		t.Fatal(err)
	}
	var pendingLocked *time.Time
	if err := pool.QueryRow(ctx, `SELECT locked_at FROM jobs WHERE id = $1`, pendingID).Scan(&pendingLocked); err != nil {
		t.Fatal(err)
	}
	if pendingLocked != nil {
		t.Fatalf("heartbeat on pending set locked_at to %v", pendingLocked)
	}
	if gotPending, err := q.Get(ctx, pendingID); err != nil || gotPending != nil {
		t.Fatalf("Get on pending job = %#v, err = %v; want nil, nil", gotPending, err)
	}

	job, err := q.Claim(ctx, "worker-hb", 5*time.Second)
	if err != nil || job == nil || job.RunID != runID || job.Step != run.StepPlan {
		t.Fatalf("claim: job=%#v err=%v", job, err)
	}
	gotRunning, err := q.Get(ctx, job.ID)
	if err != nil || gotRunning == nil || *gotRunning != *job {
		t.Fatalf("Get on running job = %#v, err = %v; want %#v", gotRunning, err, job)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET locked_at = now() - interval '1 minute' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.Heartbeat(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	var lockedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT locked_at FROM jobs WHERE id = $1`, job.ID).Scan(&lockedAt); err != nil {
		t.Fatal(err)
	}
	if time.Since(lockedAt) > 2*time.Second {
		t.Fatalf("heartbeat locked_at %v is stale", lockedAt)
	}

	if err := q.Complete(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if gotCompleted, err := q.Get(ctx, job.ID); err != nil || gotCompleted != nil {
		t.Fatalf("Get on completed job = %#v, err = %v; want nil, nil", gotCompleted, err)
	}
	var status string
	var completeLocked *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, locked_at FROM jobs WHERE id = $1`, job.ID).Scan(&status, &completeLocked); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || completeLocked != nil {
		t.Fatalf("complete status=%q locked_at=%v", status, completeLocked)
	}
}

func clearJobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
}

func containsJob(jobs []Job, id int64) bool {
	for _, j := range jobs {
		if j.ID == id {
			return true
		}
	}
	return false
}

func jobLastError(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID int64) string {
	t.Helper()
	var lastError *string
	if err := pool.QueryRow(ctx, `SELECT last_error FROM jobs WHERE id = $1`, jobID).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	if lastError == nil {
		return ""
	}
	return *lastError
}
