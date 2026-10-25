package projectapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"infra-guy/internal/domain/project"
)

var ErrActiveRun = errors.New("cannot force-unlock while a run is active")

type StateForceUnlocker interface {
	ForceUnlock(ctx context.Context, projectID string) error
}

type Service struct {
	Projects project.Repository
	States   StateForceUnlocker
	Now      func() time.Time
	NewID    func() string
}

type CreateInput struct {
	TenantID         string
	Name             string
	Repo             project.SourceRepo
	GitHubRepoID     int64
	Branch           string
	Workdir          string
	TerraformVersion string
	ApprovalPolicyID string
	AutoApprove      bool
}

type UpdateInput struct {
	ID                   project.ID
	Name                 string
	Branch               string
	Workdir              string
	TerraformVersion     string
	ApprovalPolicyID     string
	AutoApprove          bool
	Tags                 map[string]string
	UpdateDrift          bool
	DriftEnabled         bool
	DriftIntervalMinutes int
	UpdateTTL            bool
	TTLHours             int
	AutoDestroy          bool
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
	if in.ApprovalPolicyID != "" {
		p.ChangeApprovalPolicy(in.ApprovalPolicyID)
	}
	if in.Tags != nil {
		if err := p.SetTags(in.Tags); err != nil {
			return nil, err
		}
	}
	p.SetAutoApprove(in.AutoApprove)
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	if in.UpdateDrift {
		p.SetDriftConfig(in.DriftEnabled, in.DriftIntervalMinutes, now)
	}
	if in.UpdateTTL {
		p.SetTTL(in.TTLHours, in.AutoDestroy, now)
	}
	if err := s.Projects.Save(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Service) SetDriftConfig(ctx context.Context, id project.ID, enabled bool, intervalMinutes int) error {
	p, err := s.Projects.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("project %s not found", id)
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	p.SetDriftConfig(enabled, intervalMinutes, now)
	return s.Projects.Save(ctx, p)
}

func (s *Service) SetTTL(ctx context.Context, id project.ID, ttlHours int, autoDestroy bool) error {
	p, err := s.Projects.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("project %s not found", id)
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	p.SetTTL(ttlHours, autoDestroy, now)
	return s.Projects.Save(ctx, p)
}

func (s *Service) ExtendTTL(ctx context.Context, id project.ID, extraHours int) (*project.Project, error) {
	p, err := s.Projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("project %s not found", id)
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	p.ExtendTTL(extraHours, now)
	if err := s.Projects.Save(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Service) SetTags(ctx context.Context, id project.ID, tags map[string]string) error {
	p, err := s.Projects.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("project %s not found", id)
	}
	if err := p.SetTags(tags); err != nil {
		return err
	}
	return s.Projects.Save(ctx, p)
}

func (s *Service) UpdateApprovalPolicy(ctx context.Context, id project.ID, policyID string) error {
	p, err := s.Projects.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("project %s not found", id)
	}
	p.ChangeApprovalPolicy(policyID)
	return s.Projects.Save(ctx, p)
}

func (s *Service) SetApprovalPolicyID(ctx context.Context, id project.ID, policyID string) error {
	return s.UpdateApprovalPolicy(ctx, id, policyID)
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

func (s *Service) ForceUnlock(ctx context.Context, id project.ID) error {
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
	if s.States == nil {
		return nil
	}
	return s.States.ForceUnlock(ctx, string(id))
}

func (s *Service) SyncAndFindByRepo(ctx context.Context, tenantID string, repoID int64, ownerLogin, repoName string) ([]*project.Project, error) {
	matched, err := s.Projects.FindByRepo(ctx, tenantID, repoID)
	if err != nil {
		return nil, err
	}
	if ownerLogin != "" && repoName != "" {
		now := time.Now().UTC()
		if s.Now != nil {
			now = s.Now()
		}
		for _, p := range matched {
			snap := p.Snapshot()
			if snap.Repo.Owner != ownerLogin || snap.Repo.Name != repoName {
				if err := p.RenameRepo(ownerLogin, repoName, now); err != nil {
					return nil, err
				}
				if err := s.Projects.Save(ctx, p); err != nil {
					return nil, err
				}
			}
		}
	}
	return matched, nil
}

