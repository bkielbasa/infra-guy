package run

import (
	"errors"
	"fmt"
	"strings"
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
	TriggerPipeline    Trigger = "pipeline"
	TriggerDrift       Trigger = "drift"
	TriggerTTL         Trigger = "ttl"
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
	PlanJSON []byte
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
	IsDestroy   bool
	Trigger     Trigger
	SHA         string
	Branch      string
	Message     string
	Author      string
	TriggeredBy string
	PRNumber    *int
	PRTitle     string
	CommittedAt *time.Time
	Now         time.Time
}

type Approval struct {
	UserID     string
	Login      string
	Role       string
	ApprovedAt time.Time
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
	IsDestroy        bool
	PlanText         string
	PlanFile         []byte
	PlanJSON         []byte
	Add              int
	Change           int
	Destroy          int
	SummaryPresent   bool
	MonthlyCost      float64
	DiffMonthlyCost  float64
	TerraformVersion string
	Error            string
	ApprovedBy       string
	ApprovedAt       *time.Time
	Approvals        []Approval
	CommittedAt      *time.Time
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
	var committedAt *time.Time
	if in.CommittedAt != nil && !in.CommittedAt.IsZero() {
		ts := in.CommittedAt.UTC()
		committedAt = &ts
	}
	return &Run{s: Snapshot{
		ID: in.ID, ProjectID: in.ProjectID, Trigger: in.Trigger, Status: StatusQueued,
		SHA: in.SHA, Branch: in.Branch, Message: in.Message, Author: in.Author,
		TriggeredBy: in.TriggeredBy, PRNumber: in.PRNumber, PRTitle: in.PRTitle,
		PlanOnly: in.PlanOnly, AutoApprove: in.AutoApprove, IsDestroy: in.IsDestroy,
		CommittedAt: committedAt, CreatedAt: in.Now, Steps: steps,
	}}, nil
}

func Restore(s Snapshot) *Run {
	s.Steps = append([]Step(nil), s.Steps...)
	s.Approvals = append([]Approval(nil), s.Approvals...)
	s.PlanFile = append([]byte(nil), s.PlanFile...)
	s.PlanJSON = append([]byte(nil), s.PlanJSON...)
	return &Run{s: s}
}

func (r *Run) Snapshot() Snapshot {
	out := r.s
	out.Steps = append([]Step(nil), r.s.Steps...)
	out.Approvals = append([]Approval(nil), r.s.Approvals...)
	out.PlanFile = append([]byte(nil), r.s.PlanFile...)
	out.PlanJSON = append([]byte(nil), r.s.PlanJSON...)
	return out
}

func (r *Run) PlanJSON() []byte {
	return append([]byte(nil), r.s.PlanJSON...)
}

func (s Snapshot) HasChanges() bool {
	return s.Add > 0 || s.Change > 0 || s.Destroy > 0
}

func (s Snapshot) SummaryBracket() string {
	var parts []string
	if s.Add > 0 {
		parts = append(parts, fmt.Sprintf("add: %d", s.Add))
	}
	if s.Change > 0 {
		parts = append(parts, fmt.Sprintf("change: %d", s.Change))
	}
	if s.Destroy > 0 {
		parts = append(parts, fmt.Sprintf("delete: %d", s.Destroy))
	}
	if s.DiffMonthlyCost != 0 {
		parts = append(parts, fmt.Sprintf("cost: %s", FormatCostDiff(s.DiffMonthlyCost)))
	}
	if len(parts) == 0 {
		return "No changes"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (s Snapshot) CostEstimate() *CostEstimate {
	if len(s.PlanJSON) > 0 {
		if est := EstimatePlanCost(s.PlanJSON); est != nil && est.HasCost() {
			return est
		}
	}
	if s.MonthlyCost > 0 || s.DiffMonthlyCost != 0 {
		return &CostEstimate{
			PrevMonthlyCost: roundCents(s.MonthlyCost - s.DiffMonthlyCost),
			MonthlyCost:     s.MonthlyCost,
			DiffMonthlyCost: s.DiffMonthlyCost,
			Currency:        "USD",
		}
	}
	return nil
}

func (r *Run) SetTerraformVersion(v string) { r.s.TerraformVersion = v }

func (r *Run) SetStepLogs(name StepName, logs string) error {
	st := r.step(name)
	if st == nil {
		return fmt.Errorf("unknown step %s", name)
	}
	st.Logs = logs
	return nil
}

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
		if len(result.PlanJSON) > 0 {
			r.s.PlanJSON = append([]byte(nil), result.PlanJSON...)
			if cost := EstimatePlanCost(result.PlanJSON); cost != nil {
				r.s.MonthlyCost = cost.MonthlyCost
				r.s.DiffMonthlyCost = cost.DiffMonthlyCost
			}
		}
		sum := ParsePlanSummary(result.PlanText)
		if !sum.Present && !sum.NoChanges() && len(result.PlanJSON) > 0 {
			if jsonSum, ok := ParsePlanSummaryJSON(result.PlanJSON); ok {
				sum = jsonSum
			}
		}
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

func (r *Run) Approve(a Approval, now time.Time) error {
	if r.s.Status != StatusNeedsApproval && !(r.s.PlanOnly && r.s.Status == StatusPlanned) {
		return ErrNotUnconfirmed
	}
	login := strings.TrimSpace(a.Login)
	userID := strings.TrimSpace(a.UserID)
	if login == "" && userID == "" {
		return fmt.Errorf("approver login or id required")
	}
	if login == "" {
		login = userID
	}
	for _, existing := range r.s.Approvals {
		if strings.EqualFold(existing.Login, login) || (userID != "" && existing.UserID == userID) {
			return nil
		}
	}
	ts := a.ApprovedAt
	if ts.IsZero() {
		ts = now
	}
	r.s.Approvals = append(r.s.Approvals, Approval{
		UserID:     userID,
		Login:      login,
		Role:       strings.TrimSpace(a.Role),
		ApprovedAt: ts.UTC(),
	})
	return nil
}

func (r *Run) Unapprove(login, userID string) error {
	if r.s.Status != StatusNeedsApproval && !(r.s.PlanOnly && r.s.Status == StatusPlanned) {
		return ErrNotUnconfirmed
	}
	login = strings.TrimSpace(login)
	userID = strings.TrimSpace(userID)
	if login == "" && userID == "" {
		return fmt.Errorf("approver login or id required")
	}
	kept := r.s.Approvals[:0]
	for _, existing := range r.s.Approvals {
		if (login != "" && strings.EqualFold(existing.Login, login)) || (userID != "" && existing.UserID == userID) {
			continue
		}
		kept = append(kept, existing)
	}
	r.s.Approvals = kept
	return nil
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
	if r.s.Status != StatusNeedsApproval && !(r.s.PlanOnly && r.s.Status == StatusPlanned) {
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

type Job struct {
	ID       int64
	RunID    ID
	Step     StepName
	Attempts int
}

type ReapDecision struct {
	Fail   bool
	Reason string
}
