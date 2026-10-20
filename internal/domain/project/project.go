package project

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrBusy              = errors.New("another run is active")
	ErrActiveRunMismatch = errors.New("active run does not match")
)

type ID string

type Snapshot struct {
	ID               ID
	Name             string
	Repo             SourceRepo
	DefaultBranch    string
	WorkingDirectory string
	AutoApprove      bool
	Locked           bool
	GitHubToken      string
	WebhookSecret    string
	StateToken       string
	ActiveRunID      string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Project struct{ s Snapshot }

func New(id ID, name string, repo SourceRepo, now time.Time) (*Project, error) {
	if id == "" || strings.TrimSpace(name) == "" || repo.Owner == "" {
		return nil, fmt.Errorf("project requires id, name, and repository")
	}
	return &Project{s: Snapshot{
		ID: id, Name: strings.TrimSpace(name), Repo: repo,
		DefaultBranch: "main", CreatedAt: now, UpdatedAt: now,
	}}, nil
}

func Restore(s Snapshot) *Project { return &Project{s: s} }

func (p *Project) Snapshot() Snapshot { return p.s }

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

func (p *Project) SetAutoApprove(v bool) { p.s.AutoApprove = v }

func (p *Project) SetGitHubToken(token string) {
	if token == "" {
		return
	}
	p.s.GitHubToken = token
}

func (p *Project) RotateWebhookSecret(secret string) { p.s.WebhookSecret = secret }
func (p *Project) SetStateToken(token string)        { p.s.StateToken = token }
func (p *Project) Lock()                             { p.s.Locked = true }
func (p *Project) Unlock()                           { p.s.Locked = false }

func (p *Project) ClaimActiveRun(runID string) error {
	if runID == "" {
		return fmt.Errorf("missing run id")
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
