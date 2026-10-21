package projectapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/state"
)

var ErrActiveRun = errors.New("cannot force-unlock while a run is active")

type Service struct {
	Projects project.Repository
	Now      func() time.Time
	NewID    func() string
}

type UpdateInput struct {
	ID               project.ID
	Name             string
	Branch           string
	Workdir          string
	TerraformVersion string
	AutoApprove      bool
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Service) List(ctx context.Context, tenantID string) ([]*project.Project, error) {
	return s.Projects.List(ctx, tenantID)
}

func (s *Service) Get(ctx context.Context, id project.ID) (*project.Project, error) {
	return s.Projects.Get(ctx, id)
}

func (s *Service) Create(ctx context.Context, tenantID, name string, repo project.SourceRepo, repoID int64, branch, workdir string, autoApprove bool) (*project.Project, error) {
	p, err := project.New(project.ID(s.NewID()), name, repo, s.Now())
	if err != nil {
		return nil, err
	}
	if err := p.SetPlacement(tenantID, repoID); err != nil {
		return nil, err
	}
	if branch != "" {
		if err := p.SetDefaultBranch(branch); err != nil {
			return nil, err
		}
	}
	if err := p.SetWorkingDirectory(workdir); err != nil {
		return nil, err
	}
	p.SetAutoApprove(autoApprove)
	token, err := randomSecret()
	if err != nil {
		return nil, err
	}
	p.SetStateToken(token)
	if err := s.Projects.Save(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Service) Update(ctx context.Context, in UpdateInput) (*project.Project, error) {
	p, err := s.Projects.Get(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("project %s not found", in.ID)
	}
	if in.Branch != "" {
		if err := p.SetDefaultBranch(in.Branch); err != nil {
			return nil, err
		}
	}
	if err := p.SetWorkingDirectory(in.Workdir); err != nil {
		return nil, err
	}
	if in.TerraformVersion != "" {
		if err := p.SetTerraformVersion(in.TerraformVersion); err != nil {
			return nil, err
		}
	}
	p.SetAutoApprove(in.AutoApprove)
	if err := s.Projects.Save(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Service) SetTerraformVersion(ctx context.Context, id project.ID, version string) error {
	p, err := s.Projects.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("project %s not found", id)
	}
	if err := p.SetTerraformVersion(version); err != nil {
		return err
	}
	return s.Projects.Save(ctx, p)
}

func (s *Service) SetLocked(ctx context.Context, id project.ID, locked bool) error {
	p, err := s.Projects.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("project %s not found", id)
	}
	return s.Projects.SetLocked(ctx, id, locked)
}

func (s *Service) Delete(ctx context.Context, id project.ID) error {
	return s.Projects.Delete(ctx, id)
}

func (s *Service) ForceUnlock(ctx context.Context, id project.ID, states state.Repository) error {
	p, err := s.Projects.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("project %s not found", id)
	}
	if p.Snapshot().ActiveRunID != "" {
		return ErrActiveRun
	}
	st, err := states.Load(ctx, string(id))
	if err != nil {
		return err
	}
	if st == nil {
		return nil
	}
	st.ForceUnlock()
	return states.Save(ctx, st)
}
