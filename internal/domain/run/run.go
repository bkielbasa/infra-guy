package run

import (
	"errors"
	"fmt"
	"time"
)

var ErrNotUnconfirmed = errors.New("run is not awaiting confirmation")

type ID string
type ProjectID string

type Status string

const (
	StatusQueued        Status = "queued"
	StatusRunning       Status = "running"
	StatusNeedsApproval Status = "needs_approval"
	StatusPlanned       Status = "planned"
	StatusSucceeded     Status = "succeeded"
	StatusFailed        Status = "failed"
	StatusCancelled     Status = "cancelled"
)

func (s Status) Label() string {
	switch s {
	case StatusNeedsApproval:
		return "unconfirmed"
	case StatusSucceeded:
		return "finished"
	default:
		return string(s)
	}
}

func (s Status) Terminal() bool {
	switch s {
	case StatusPlanned, StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

type Trigger string

const (
	TriggerPullRequest Trigger = "pull_request"
	TriggerPush        Trigger = "push"
	TriggerManual      Trigger = "manual"
)

type StepName string

const (
	StepInit  StepName = "init"
	StepPlan  StepName = "plan"
	StepApply StepName = "apply"
)

type StepStatus string

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepSucceeded StepStatus = "succeeded"
	StepFailed    StepStatus = "failed"
	StepSkipped   StepStatus = "skipped"
)

type DecisionKind string

const (
	DecisionEnqueue         DecisionKind = "enqueue"
	DecisionWaitApproval    DecisionKind = "wait_approval"
	DecisionFinishPlanned   DecisionKind = "finish_planned"
	DecisionFinishNoChanges DecisionKind = "finish_no_changes"
	DecisionFinished        DecisionKind = "finished"
	DecisionFail            DecisionKind = "fail"
)

type Decision struct {
	Kind DecisionKind
	Step StepName
}

type StepResult struct {
	ExitCode int
	Logs     string
	PlanText string
	PlanFile []byte
}

type Step struct {
	Name       StepName
	Position   int
	Status     StepStatus
	Logs       string
	ExitCode   *int
	StartedAt  *time.Time
	FinishedAt *time.Time
}

type OpenInput struct {
	ID          ID
	ProjectID   ProjectID
	AutoApprove bool
	PlanOnly    bool
	Trigger     Trigger
	SHA         string
	Branch      string
	Message     string
	Author      string
	TriggeredBy string
	PRNumber    *int
	PRTitle     string
	Now         time.Time
}

type Snapshot struct {
	ID               ID
	ProjectID        ProjectID
	Trigger          Trigger
	Status           Status
	SHA              string
	Branch           string
	Message          string
	Author           string
	TriggeredBy      string
	PRNumber         *int
	PRTitle          string
	PlanOnly         bool
	AutoApprove      bool
	PlanText         string
	PlanFile         []byte
	Add              int
	Change           int
	Destroy          int
	SummaryPresent   bool
	TerraformVersion string
	Error            string
	ApprovedBy       string
	ApprovedAt       *time.Time
	CreatedAt        time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
	Steps            []Step
}

type Run struct{ s Snapshot }

func Open(in OpenInput) (*Run, error) {
	if in.ID == "" || in.ProjectID == "" || in.SHA == "" {
		return nil, fmt.Errorf("run requires id, project, and sha")
	}
	steps := []Step{
		{Name: StepInit, Position: 1, Status: StepPending},
		{Name: StepPlan, Position: 2, Status: StepPending},
	}
	if !in.PlanOnly {
		steps = append(steps, Step{Name: StepApply, Position: 3, Status: StepPending})
	}
	return &Run{s: Snapshot{
		ID: in.ID, ProjectID: in.ProjectID, Trigger: in.Trigger, Status: StatusQueued,
		SHA: in.SHA, Branch: in.Branch, Message: in.Message, Author: in.Author,
		TriggeredBy: in.TriggeredBy, PRNumber: in.PRNumber, PRTitle: in.PRTitle,
		PlanOnly: in.PlanOnly, AutoApprove: in.AutoApprove, CreatedAt: in.Now, Steps: steps,
	}}, nil
}

func Restore(s Snapshot) *Run { return &Run{s: s} }

func (r *Run) Snapshot() Snapshot {
	out := r.s
	out.Steps = append([]Step(nil), r.s.Steps...)
	out.PlanFile = append([]byte(nil), r.s.PlanFile...)
	return out
}

func (r *Run) SetTerraformVersion(v string) { r.s.TerraformVersion = v }

func (r *Run) BeginStep(name StepName, now time.Time) error {
	if r.s.Status.Terminal() {
		return fmt.Errorf("run is %s", r.s.Status)
	}
	st := r.step(name)
	if st == nil {
		return fmt.Errorf("unknown step %s", name)
	}
	if st.Status != StepPending {
		return fmt.Errorf("step %s is %s", name, st.Status)
	}
	st.Status = StepRunning
	st.StartedAt = &now
	if r.s.Status == StatusQueued {
		r.s.Status = StatusRunning
		r.s.StartedAt = &now
	}
	return nil
}

func (r *Run) CompleteStep(name StepName, result StepResult, now time.Time) (Decision, error) {
	if r.s.Status.Terminal() {
		return Decision{}, fmt.Errorf("run is %s", r.s.Status)
	}
	st := r.step(name)
	if st == nil {
		return Decision{}, fmt.Errorf("unknown step %s", name)
	}
	if st.Status != StepRunning {
		return Decision{}, fmt.Errorf("step %s is not running", name)
	}
	st.Logs = result.Logs
	code := result.ExitCode
	st.ExitCode = &code
	st.FinishedAt = &now
	if result.ExitCode != 0 {
		st.Status = StepFailed
		r.s.Status = StatusFailed
		r.s.FinishedAt = &now
		r.s.Error = fmt.Sprintf("%s exited %d", name, result.ExitCode)
		return Decision{Kind: DecisionFail}, nil
	}
	st.Status = StepSucceeded
	switch name {
	case StepInit:
		return Decision{Kind: DecisionEnqueue, Step: StepPlan}, nil
	case StepPlan:
		r.s.PlanText = result.PlanText
		r.s.PlanFile = append([]byte(nil), result.PlanFile...)
		sum := ParsePlanSummary(result.PlanText)
		r.s.SummaryPresent = sum.Present
		r.s.Add, r.s.Change, r.s.Destroy = sum.Add, sum.Change, sum.Destroy
		if r.s.PlanOnly {
			r.s.Status = StatusPlanned
			r.s.FinishedAt = &now
			return Decision{Kind: DecisionFinishPlanned}, nil
		}
		if sum.NoChanges() {
			r.skip(StepApply, now)
			r.s.Status = StatusSucceeded
			r.s.FinishedAt = &now
			return Decision{Kind: DecisionFinishNoChanges}, nil
		}
		if r.s.AutoApprove {
			return Decision{Kind: DecisionEnqueue, Step: StepApply}, nil
		}
		r.s.Status = StatusNeedsApproval
		return Decision{Kind: DecisionWaitApproval}, nil
	case StepApply:
		r.s.Status = StatusSucceeded
		r.s.FinishedAt = &now
		return Decision{Kind: DecisionFinished}, nil
	default:
		return Decision{}, fmt.Errorf("unknown step %s", name)
	}
}

func (r *Run) Confirm(actor string, now time.Time) (Decision, error) {
	if r.s.Status != StatusNeedsApproval {
		return Decision{}, ErrNotUnconfirmed
	}
	r.s.ApprovedBy = actor
	r.s.ApprovedAt = &now
	r.s.Status = StatusRunning
	return Decision{Kind: DecisionEnqueue, Step: StepApply}, nil
}

func (r *Run) Cancel(actor string, now time.Time) error {
	if r.s.Status != StatusNeedsApproval {
		return ErrNotUnconfirmed
	}
	r.skip(StepApply, now)
	r.s.ApprovedBy = actor
	r.s.ApprovedAt = &now
	r.s.Status = StatusCancelled
	r.s.FinishedAt = &now
	return nil
}

func (r *Run) Fail(name StepName, message string, now time.Time) error {
	if st := r.step(name); st != nil && st.Status != StepSucceeded && st.Status != StepSkipped {
		st.Status = StepFailed
		st.FinishedAt = &now
		if st.Logs != "" {
			st.Logs += "\n"
		}
		st.Logs += message
	}
	r.s.Status = StatusFailed
	r.s.FinishedAt = &now
	r.s.Error = message
	return nil
}

func (r *Run) step(name StepName) *Step {
	for i := range r.s.Steps {
		if r.s.Steps[i].Name == name {
			return &r.s.Steps[i]
		}
	}
	return nil
}

func (r *Run) skip(name StepName, now time.Time) {
	if st := r.step(name); st != nil {
		st.Status = StepSkipped
		st.FinishedAt = &now
	}
}
