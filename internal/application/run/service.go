package runapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
)

var ErrDuplicate = errors.New("run already queued or running for this commit")

type Scheduler interface {
	Enqueue(ctx context.Context, runID run.ID, step run.StepName) error
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
	Projects project.Repository
	Runs     run.Repository
	Schedule Scheduler
	Now      func() time.Time
	NewID    func() string
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
	return s.apply(ctx, r, d)
}

func (s *Service) Cancel(ctx context.Context, id run.ID) error {
	r, err := s.Runs.Get(ctx, id)
	if err != nil || r == nil {
		return fmt.Errorf("run %s not found", id)
	}
	if err := r.Cancel(s.Now()); err != nil {
		return err
	}
	if err := s.Runs.Save(ctx, r); err != nil {
		return err
	}
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
	return s.releaseIfTerminal(ctx, r)
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
	return s.Projects.ReleaseActiveRun(ctx, project.ID(rs.ProjectID), string(rs.ID))
}
