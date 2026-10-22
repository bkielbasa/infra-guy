package runapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
)

var ErrDuplicate = errors.New("run already queued or running for this commit")

type Scheduler interface {
	Enqueue(ctx context.Context, runID run.ID, step run.StepName) error
}

type StatusReport struct {
	State       string
	Description string
	Context     string
	TargetURL   string
}

type StatusReporter interface {
	ReportCommitStatus(ctx context.Context, p project.Snapshot, sha string, status StatusReport) error
}

type MergeabilityChecker interface {
	IsMergeable(ctx context.Context, p project.Snapshot, r run.Snapshot) bool
}

type OpenRequest struct {
	ProjectID   project.ID
	Trigger     run.Trigger
	SHA         string
	Branch      string
	Message     string
	Author      string
	TriggeredBy string
	PRTitle     string
	PRNumber    *int
	PlanOnly    bool
}

type Service struct {
	Projects       project.Repository
	Runs           run.Repository
	Schedule       Scheduler
	StatusReporter StatusReporter
	Mergeability   MergeabilityChecker
	PublicBaseURL  string
	Now            func() time.Time
	NewID          func() string
	OnTerminal     func(ctx context.Context, p *project.Project, r *run.Run)
}

func (s *Service) Get(ctx context.Context, id run.ID) (*run.Run, error) {
	return s.Runs.Get(ctx, id)
}

func (s *Service) ListByProject(ctx context.Context, projectID project.ID) ([]*run.Run, error) {
	return s.Runs.ListByProject(ctx, run.ProjectID(projectID))
}

func (s *Service) Open(ctx context.Context, in OpenRequest) (*run.Run, error) {
	p, err := s.Projects.Get(ctx, in.ProjectID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("project %s not found", in.ProjectID)
	}
	opened, err := run.Open(run.OpenInput{
		ID: run.ID(s.NewID()), ProjectID: run.ProjectID(in.ProjectID),
		AutoApprove: p.Snapshot().AutoApprove, PlanOnly: in.PlanOnly, Trigger: in.Trigger,
		SHA: in.SHA, Branch: in.Branch, Message: in.Message, Author: in.Author,
		TriggeredBy: in.TriggeredBy, PRNumber: in.PRNumber, PRTitle: in.PRTitle, Now: s.Now(),
	})
	if err != nil {
		return nil, err
	}
	ok, err := s.Runs.InsertIfAbsent(ctx, opened)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrDuplicate
	}
	if err := s.enqueue(ctx, opened.Snapshot().ID, run.StepInit); err != nil {
		return nil, err
	}
	s.reportPRStatus(ctx, opened)
	return opened, nil
}

// enqueue fails the run when the job cannot be queued, so the run never sits
// queued without a job and a retry is not rejected as a duplicate.
func (s *Service) enqueue(ctx context.Context, id run.ID, step run.StepName) error {
	err := s.Schedule.Enqueue(ctx, id, step)
	if err == nil {
		return nil
	}
	if ferr := s.Fail(ctx, id, step, "could not queue "+string(step)+": "+err.Error()); ferr != nil {
		return errors.Join(err, ferr)
	}
	return err
}

func (s *Service) Approve(ctx context.Context, id run.ID, a run.Approval) error {
	r, err := s.Runs.Get(ctx, id)
	if err != nil || r == nil {
		return fmt.Errorf("run %s not found", id)
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	if err := r.Approve(a, now); err != nil {
		return err
	}
	if err := s.Runs.Save(ctx, r); err != nil {
		return err
	}
	s.reportPRStatus(ctx, r)
	return nil
}

func (s *Service) Confirm(ctx context.Context, id run.ID, actor string) error {
	r, err := s.Runs.Get(ctx, id)
	if err != nil || r == nil {
		return fmt.Errorf("run %s not found", id)
	}
	d, err := r.Confirm(actor, s.Now())
	if err != nil {
		return err
	}
	if err := s.Runs.Save(ctx, r); err != nil {
		return err
	}
	s.reportPRStatus(ctx, r)
	return s.apply(ctx, r, d)
}

func (s *Service) Cancel(ctx context.Context, id run.ID, actor string) error {
	r, err := s.Runs.Get(ctx, id)
	if err != nil || r == nil {
		return fmt.Errorf("run %s not found", id)
	}
	if err := r.Cancel(actor, s.Now()); err != nil {
		return err
	}
	if err := s.Runs.Save(ctx, r); err != nil {
		return err
	}
	s.reportPRStatus(ctx, r)
	return s.releaseIfTerminal(ctx, r)
}

func (s *Service) Advance(ctx context.Context, id run.ID, step run.StepName, result run.StepResult) error {
	r, err := s.Runs.Get(ctx, id)
	if err != nil || r == nil {
		return fmt.Errorf("run %s not found", id)
	}
	d, err := r.CompleteStep(step, result, s.Now())
	if err != nil {
		return err
	}
	if err := s.Runs.Save(ctx, r); err != nil {
		return err
	}
	if step == run.StepPlan || r.Snapshot().Status.Terminal() {
		s.reportPRStatus(ctx, r)
	}
	return s.apply(ctx, r, d)
}

func (s *Service) SetTerraformVersion(ctx context.Context, id run.ID, version string) error {
	r, err := s.Runs.Get(ctx, id)
	if err != nil || r == nil {
		return fmt.Errorf("run %s not found", id)
	}
	r.SetTerraformVersion(version)
	return s.Runs.Save(ctx, r)
}

func (s *Service) UpdateStepLogs(ctx context.Context, id run.ID, step run.StepName, logs string) error {
	r, err := s.Runs.Get(ctx, id)
	if err != nil || r == nil {
		return fmt.Errorf("run %s not found", id)
	}
	if err := r.SetStepLogs(step, logs); err != nil {
		return err
	}
	return s.Runs.Save(ctx, r)
}

func (s *Service) Fail(ctx context.Context, id run.ID, step run.StepName, message string) error {
	r, err := s.Runs.Get(ctx, id)
	if err != nil || r == nil {
		return fmt.Errorf("run %s not found", id)
	}
	if err := r.Fail(step, message, s.Now()); err != nil {
		return err
	}
	if err := s.Runs.Save(ctx, r); err != nil {
		return err
	}
	s.reportPRStatus(ctx, r)
	return s.releaseIfTerminal(ctx, r)
}

func (s *Service) reportPRStatus(ctx context.Context, r *run.Run) {
	if s.StatusReporter == nil || s.Projects == nil {
		return
	}
	rs := r.Snapshot()
	if rs.Trigger != run.TriggerPullRequest || rs.SHA == "" {
		return
	}
	p, err := s.Projects.Get(ctx, project.ID(rs.ProjectID))
	if err != nil || p == nil {
		return
	}
	ps := p.Snapshot()
	var targetURL string
	if s.PublicBaseURL != "" {
		targetURL = strings.TrimRight(s.PublicBaseURL, "/") + "/runs/" + string(rs.ID)
	}
	report := StatusReport{
		Context:   "infra-guy/" + ps.Name,
		TargetURL: targetURL,
	}
	switch rs.Status {
	case run.StatusQueued, run.StatusRunning:
		report.State = "pending"
		report.Description = "In progress"
	case run.StatusFailed:
		report.State = "failure"
		report.Description = "Plan failed"
	case run.StatusCancelled:
		report.State = "failure"
		report.Description = "Cancelled"
	case run.StatusPlanned, run.StatusNeedsApproval, run.StatusSucceeded:
		if !rs.HasChanges() {
			report.State = "success"
			report.Description = "No changes"
		} else {
			report.Description = rs.SummaryBracket()
			if s.isMergeable(ctx, ps, rs) {
				report.State = "success"
			} else {
				report.State = "pending"
			}
		}
	default:
		return
	}
	_ = s.StatusReporter.ReportCommitStatus(ctx, ps, rs.SHA, report)
}

func (s *Service) isMergeable(ctx context.Context, ps project.Snapshot, rs run.Snapshot) bool {
	if s.Mergeability != nil {
		return s.Mergeability.IsMergeable(ctx, ps, rs)
	}
	return ps.AutoApprove || len(rs.Approvals) > 0
}

func (s *Service) apply(ctx context.Context, r *run.Run, d run.Decision) error {
	if d.Kind == run.DecisionEnqueue {
		return s.enqueue(ctx, r.Snapshot().ID, d.Step)
	}
	return s.releaseIfTerminal(ctx, r)
}

func (s *Service) releaseIfTerminal(ctx context.Context, r *run.Run) error {
	rs := r.Snapshot()
	if !rs.Status.Terminal() {
		return nil
	}
	if s.OnTerminal != nil {
		if p, err := s.Projects.Get(ctx, project.ID(rs.ProjectID)); err == nil && p != nil {
			s.OnTerminal(ctx, p, r)
		}
	}
	return s.Projects.ReleaseActiveRun(ctx, project.ID(rs.ProjectID), string(rs.ID))
}
