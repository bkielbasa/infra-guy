package project

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const DefaultTerraformVersion = "1.9.8"

var (
	ErrBusy                    = errors.New("another run is active")
	ErrLocked                  = errors.New("project is locked")
	ErrActiveRunMismatch       = errors.New("active run does not match")
	ErrInvalidTerraformVersion = errors.New("invalid terraform version")
	ErrInvalidTag              = errors.New("invalid tag: expected key:value")
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
	Tags             map[string]string
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
	Tags             map[string]string
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
	s.Tags = cloneTags(s.Tags)
	return &Project{s: s}
}

func Rehydrate(s Snapshot) *Project { return Restore(s) }

func (p *Project) Snapshot() Snapshot {
	s := p.s
	s.Tags = cloneTags(p.s.Tags)
	return s
}

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

func (p *Project) SetTags(tags map[string]string) error {
	cleaned := make(map[string]string, len(tags))
	for k, v := range tags {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !validTagPart(k) || !validTagPart(v) {
			return fmt.Errorf("%w: %q:%q", ErrInvalidTag, k, v)
		}
		cleaned[k] = v
	}
	p.s.Tags = cleaned
	return nil
}

func ParseTags(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, token := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		k, v, ok := strings.Cut(token, ":")
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrInvalidTag, token)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !validTagPart(k) || !validTagPart(v) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidTag, token)
		}
		out[k] = v
	}
	return out, nil
}

func TagList(tags map[string]string) []string {
	if len(tags) == 0 {
		return nil
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+":"+tags[k])
	}
	return out
}

func FormatTags(tags map[string]string) string {
	return strings.Join(TagList(tags), ", ")
}

func validTagPart(s string) bool {
	if s == "" {
		return false
	}
	return !strings.ContainsAny(s, " \t\n\r:,")
}

func cloneTags(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

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

