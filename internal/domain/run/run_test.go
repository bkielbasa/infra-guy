package run

import (
	"testing"
	"time"
)

func openFull(auto bool) *Run {
	r, err := Open(OpenInput{
		ID: "run-1", ProjectID: "proj-1", AutoApprove: auto,
		Trigger: TriggerPush, SHA: "abc", Branch: "main", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		panic(err)
	}
	return r
}

func beginStep(t *testing.T, r *Run, name StepName, now time.Time) {
	t.Helper()
	if err := r.BeginStep(name, now); err != nil {
		t.Fatal(err)
	}
}

func completeStep(t *testing.T, r *Run, name StepName, result StepResult, now time.Time) Decision {
	t.Helper()
	d, err := r.CompleteStep(name, result, now)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOpenPlanOnlyHasNoApply(t *testing.T) {
	r, err := Open(OpenInput{
		ID: "run-1", ProjectID: "proj-1", PlanOnly: true,
		Trigger: TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := r.Snapshot()
	if len(snap.Steps) != 2 || snap.Steps[0].Name != StepInit || snap.Steps[1].Name != StepPlan {
		t.Fatalf("steps: %#v", snap.Steps)
	}
	if snap.Status != StatusQueued || !snap.PlanOnly || snap.AutoApprove {
		t.Fatalf("%#v", snap)
	}
}

func TestCompleteInitEnqueuesPlan(t *testing.T) {
	r := openFull(true)
	now := time.Unix(10, 0).UTC()
	beginStep(t, r, StepInit, now)
	d := completeStep(t, r, StepInit, StepResult{Logs: "ok"}, now)
	if d.Kind != DecisionEnqueue || d.Step != StepPlan {
		t.Fatalf("%#v", d)
	}
}

func TestPlanOnlyFinishesPlanned(t *testing.T) {
	r, err := Open(OpenInput{ID: "r", ProjectID: "p", PlanOnly: true, Trigger: TriggerPullRequest, SHA: "abc", Now: time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10, 0).UTC()
	beginStep(t, r, StepInit, now)
	completeStep(t, r, StepInit, StepResult{}, now)
	beginStep(t, r, StepPlan, now)
	d, err := r.CompleteStep(StepPlan, StepResult{
		PlanText: "Plan: 2 to add, 0 to change, 0 to destroy.\n",
		PlanFile: []byte("bin"),
		PlanJSON: []byte(`{"format_version":"1.2"}`),
	}, now)
	if err != nil || d.Kind != DecisionFinishPlanned {
		t.Fatalf("%v %#v", err, d)
	}
	if r.Snapshot().Status != StatusPlanned || string(r.Snapshot().PlanFile) != "bin" || string(r.Snapshot().PlanJSON) != `{"format_version":"1.2"}` {
		t.Fatalf("%#v", r.Snapshot())
	}
}

func TestNoChangesSkipsApply(t *testing.T) {
	r := openFull(false)
	now := time.Unix(10, 0).UTC()
	beginStep(t, r, StepInit, now)
	completeStep(t, r, StepInit, StepResult{}, now)
	beginStep(t, r, StepPlan, now)
	d, err := r.CompleteStep(StepPlan, StepResult{PlanText: "Plan: 0 to add, 0 to change, 0 to destroy.\n"}, now)
	if err != nil || d.Kind != DecisionFinishNoChanges {
		t.Fatalf("%v %#v", err, d)
	}
	snap := r.Snapshot()
	if snap.Status != StatusSucceeded || snap.Steps[2].Status != StepSkipped {
		t.Fatalf("%#v", snap.Steps)
	}
}

func TestAutoApproveEnqueuesApply(t *testing.T) {
	r := openFull(true)
	now := time.Unix(10, 0).UTC()
	beginStep(t, r, StepInit, now)
	completeStep(t, r, StepInit, StepResult{}, now)
	beginStep(t, r, StepPlan, now)
	d := completeStep(t, r, StepPlan, StepResult{PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.\n"}, now)
	if d.Kind != DecisionEnqueue || d.Step != StepApply {
		t.Fatalf("%#v", d)
	}
}

func TestConfirmAndCancel(t *testing.T) {
	r := openFull(false)
	now := time.Unix(10, 0).UTC()
	beginStep(t, r, StepInit, now)
	completeStep(t, r, StepInit, StepResult{}, now)
	beginStep(t, r, StepPlan, now)
	d := completeStep(t, r, StepPlan, StepResult{PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.\n"}, now)
	if d.Kind != DecisionWaitApproval || r.Snapshot().Status != StatusNeedsApproval {
		t.Fatalf("status %s decision %#v", r.Snapshot().Status, d)
	}
	if _, err := r.Confirm("operator", now); err != nil {
		t.Fatal(err)
	}
	if r.Snapshot().ApprovedBy != "operator" || r.Snapshot().Status != StatusRunning {
		t.Fatalf("%#v", r.Snapshot())
	}
	again := openFull(false)
	beginStep(t, again, StepInit, now)
	completeStep(t, again, StepInit, StepResult{}, now)
	beginStep(t, again, StepPlan, now)
	completeStep(t, again, StepPlan, StepResult{PlanText: "Plan: 1 to add, 0 to change, 0 to destroy.\n"}, now)
	if err := again.Cancel("ada", now); err != nil {
		t.Fatal(err)
	}
	snap := again.Snapshot()
	if snap.Status != StatusCancelled || snap.Steps[2].Status != StepSkipped || snap.ApprovedBy != "ada" || snap.ApprovedAt == nil || !snap.ApprovedAt.Equal(now) {
		t.Fatalf("%#v", snap)
	}
	if err := again.Cancel("ada", now); err != ErrNotUnconfirmed {
		t.Fatalf("second cancel: %v", err)
	}
	if _, err := again.Confirm("operator", now); err != ErrNotUnconfirmed {
		t.Fatalf("confirm after cancel: %v", err)
	}
}

func TestNonZeroExitFailsAndDoesNotEnqueue(t *testing.T) {
	r := openFull(true)
	now := time.Unix(10, 0).UTC()
	beginStep(t, r, StepInit, now)
	d, err := r.CompleteStep(StepInit, StepResult{ExitCode: 1, Logs: "boom"}, now)
	if err != nil || d.Kind != DecisionFail {
		t.Fatalf("%v %#v", err, d)
	}
	if r.Snapshot().Status != StatusFailed || !r.Snapshot().Status.Terminal() {
		t.Fatal(r.Snapshot().Status)
	}
	if err := r.BeginStep(StepPlan, now); err == nil {
		t.Fatal("expected error beginning plan after failed init")
	}
	if r.Snapshot().Status != StatusFailed {
		t.Fatalf("status changed: %s", r.Snapshot().Status)
	}
}

func TestLabel(t *testing.T) {
	if StatusNeedsApproval.Label() != "unconfirmed" || StatusSucceeded.Label() != "finished" || StatusQueued.Label() != "queued" {
		t.Fatal("labels")
	}
}
