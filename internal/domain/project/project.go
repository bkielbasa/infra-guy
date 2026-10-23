package project

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const DefaultTerraformVersion = "1.9.8"

var (
	ErrBusy                    = errors.New("another run is active")
	ErrLocked                  = errors.New("project is locked")
	ErrActiveRunMismatch       = errors.New("active run does not match")
	ErrInvalidTerraformVersion = errors.New("invalid terraform version")
)

type ID string

type Snapshot struct {
	ID               ID
	TenantID         string
	GitHubRepoID     int64
	Name             string
	Repo             SourceRepo
	DefaultBranch    string
	WorkingDirectory string
	TerraformVersion string
	ApprovalPolicyID string
	AutoApprove      bool
	Locked           bool
	StateToken       string
	ActiveRunID      string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type NewInput struct {
	ID               ID
	TenantID         string
	GitHubRepoID     int64
	Name             string
	Repo             SourceRepo
	DefaultBranch    string
	WorkingDirectory string
	TerraformVersion string
	ApprovalPolicyID string
	AutoApprove      bool
	Now              time.Time
}

type Project struct{ s Snapshot }

func New(id ID, name string, repo SourceRepo, now time.Time) (*Project, error) {
	if id == "" || strings.TrimSpace(name) == "" || repo.Owner == "" {
		return nil, fmt.Errorf("project requires id, name, and repository")
	}
	return &Project{s: Snapshot{
		ID: id, Name: strings.TrimSpace(name), Repo: repo,
		DefaultBranch: "main", TerraformVersion: DefaultTerraformVersion,
		CreatedAt: now, UpdatedAt: now,
	}}, nil
}

func Restore(s Snapshot) *Project {
	s.ApprovalPolicyID = strings.TrimSpace(s.ApprovalPolicyID)
	return &Project{s: s}
}

func Rehydrate(s Snapshot) *Project { return Restore(s) }

func (p *Project) Snapshot() Snapshot { return p.s }

func (p *Project) ApprovalPolicyID() string { return p.s.ApprovalPolicyID }

func (p *Project) ChangeApprovalPolicy(policyID string) {
	p.s.ApprovalPolicyID = strings.TrimSpace(policyID)
}

func (p *Project) SetApprovalPolicyID(policyID string) {
	p.ChangeApprovalPolicy(policyID)
}

func (p *Project) SetWorkingDirectory(raw string) error {
	dir, err := ParseWorkingDirectory(raw)
	if err != nil {
		return err
	}
	p.s.WorkingDirectory = dir
	return nil
}

func (p *Project) SetDefaultBranch(branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" || strings.Contains(branch, " ") {
		return fmt.Errorf("invalid branch")
	}
	p.s.DefaultBranch = branch
	return nil
}

func ValidateTerraformVersion(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return DefaultTerraformVersion, nil
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "", ErrInvalidTerraformVersion
	}
	for _, part := range parts {
		if part == "" {
			return "", ErrInvalidTerraformVersion
		}
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return "", ErrInvalidTerraformVersion
			}
		}
	}
	return v, nil
}

func (p *Project) SetTerraformVersion(raw string) error {
	v, err := ValidateTerraformVersion(raw)
	if err != nil {
		return err
	}
	p.s.TerraformVersion = v
	return nil
}

func (p *Project) SetPlacement(tenantID string, repoID int64) error {
	if tenantID == "" || repoID <= 0 {
		return fmt.Errorf("placement requires tenant id and positive repository id")
	}
	p.s.TenantID = tenantID
	p.s.GitHubRepoID = repoID
	return nil
}

func (p *Project) SetAutoApprove(v bool) { p.s.AutoApprove = v }

func (p *Project) SetStateToken(token string)        { p.s.StateToken = token }
func (p *Project) Lock()                             { p.s.Locked = true }
func (p *Project) Unlock()                           { p.s.Locked = false }

func (p *Project) ClaimActiveRun(runID string) error {
	if runID == "" {
		return fmt.Errorf("missing run id")
	}
	if p.s.Locked {
		return ErrLocked
	}
	if p.s.ActiveRunID != "" && p.s.ActiveRunID != runID {
		return ErrBusy
	}
	p.s.ActiveRunID = runID
	return nil
}

func (p *Project) ReleaseActiveRun(runID string) error {
	if p.s.ActiveRunID != runID {
		return ErrActiveRunMismatch
	}
	p.s.ActiveRunID = ""
	return nil
}

func (p *Project) RenameRepo(owner, name string, now time.Time) error {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if owner == "" || name == "" {
		return errors.New("repo owner and name required")
	}
	p.s.Repo.Owner = owner
	p.s.Repo.Name = name
	p.s.UpdatedAt = now
	return nil
}

