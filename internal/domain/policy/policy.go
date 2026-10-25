package policy

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"infra-guy/internal/domain/project"
)

var (
	ErrInvalidID     = errors.New("policy id is required")
	ErrInvalidTenant = errors.New("policy tenant_id is required")
	ErrInvalidName   = errors.New("policy name is required")
	ErrInvalidRego   = errors.New("policy rego code is invalid")
	ErrNotFound      = errors.New("policy not found")
	ErrDuplicateName = errors.New("policy name already exists")
)

type ID string

type Snapshot struct {
	ID          ID
	TenantID    string
	Name        string
	Description string
	Rego        string
	Tags        map[string]string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type NewInput struct {
	ID          ID
	TenantID    string
	Name        string
	Description string
	Rego        string
	Tags        map[string]string
	Now         time.Time
}

type Policy struct {
	s Snapshot
}

func New(in NewInput) (*Policy, error) {
	id := ID(strings.TrimSpace(string(in.ID)))
	if id == "" {
		return nil, ErrInvalidID
	}
	tenantID := strings.TrimSpace(in.TenantID)
	if tenantID == "" {
		return nil, ErrInvalidTenant
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, ErrInvalidName
	}
	if strings.TrimSpace(in.Rego) == "" {
		return nil, ErrInvalidRego
	}
	p := &Policy{
		s: Snapshot{
			ID:          id,
			TenantID:    tenantID,
			Name:        name,
			Description: strings.TrimSpace(in.Description),
			Rego:        in.Rego,
			CreatedAt:   in.Now,
			UpdatedAt:   in.Now,
		},
	}
	if err := p.SetTags(in.Tags); err != nil {
		return nil, err
	}
	return p, nil
}

func Rehydrate(s Snapshot) *Policy {
	cp := s
	cp.Tags = cloneTags(s.Tags)
	return &Policy{s: cp}
}

func Restore(s Snapshot) *Policy {
	return Rehydrate(s)
}

func (p *Policy) Snapshot() Snapshot {
	cp := p.s
	cp.Tags = cloneTags(p.s.Tags)
	return cp
}

func (p *Policy) ID() ID                  { return p.s.ID }
func (p *Policy) TenantID() string        { return p.s.TenantID }
func (p *Policy) Name() string            { return p.s.Name }
func (p *Policy) Description() string     { return p.s.Description }
func (p *Policy) Rego() string            { return p.s.Rego }
func (p *Policy) Tags() map[string]string { return cloneTags(p.s.Tags) }
func (p *Policy) CreatedAt() time.Time    { return p.s.CreatedAt }
func (p *Policy) UpdatedAt() time.Time    { return p.s.UpdatedAt }

// MatchesProjectTags checks if a policy's tags automatically bind it to a workspace with projectTags.
// Policies without tags only apply when explicitly assigned to a workspace.
// Policies tagged with scope:global, global:true, or enforcement:global bind to all workspaces in the tenant
// (unless another tag on the policy conflicts with a workspace tag).
// Policies with key:value selector tags bind to any workspace that shares at least one matching key:value tag
// and has no conflicting value for any shared tag key.
func MatchesProjectTags(policyTags, projectTags map[string]string) (matched bool, source string, matchedTag string) {
	if len(policyTags) == 0 {
		return false, "", ""
	}
	keys := make([]string, 0, len(policyTags))
	for k := range policyTags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var globalTag string
	var selectorKeys []string
	for _, k := range keys {
		v := policyTags[k]
		kl := strings.ToLower(strings.TrimSpace(k))
		vl := strings.ToLower(strings.TrimSpace(v))
		if (kl == "scope" && vl == "global") || (kl == "global" && vl == "true") || (kl == "enforcement" && vl == "global") {
			if globalTag == "" {
				globalTag = k + ":" + v
			}
			continue
		}
		selectorKeys = append(selectorKeys, k)
	}

	for _, k := range selectorKeys {
		pv, exists := projectTags[k]
		if exists && pv != policyTags[k] {
			return false, "", ""
		}
	}

	if len(selectorKeys) == 0 && globalTag != "" {
		return true, "global", globalTag
	}

	var matchedTags []string
	for _, k := range selectorKeys {
		if pv, exists := projectTags[k]; exists && pv == policyTags[k] {
			matchedTags = append(matchedTags, k+":"+policyTags[k])
		}
	}
	if len(matchedTags) > 0 {
		return true, "tag-bound", strings.Join(matchedTags, ", ")
	}
	if globalTag != "" {
		return true, "global", globalTag
	}
	return false, "", ""
}

func (p *Policy) SetTags(tags map[string]string) error {
	norm, err := project.ParseTags(project.FormatTags(tags))
	if err != nil {
		return err
	}
	p.s.Tags = norm
	return nil
}

func (p *Policy) Update(name, description, rego string, now time.Time) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrInvalidName
	}
	if strings.TrimSpace(rego) == "" {
		return ErrInvalidRego
	}
	p.s.Name = name
	p.s.Description = strings.TrimSpace(description)
	p.s.Rego = rego
	p.s.UpdatedAt = now
	return nil
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

type Repository interface {
	Insert(ctx context.Context, p *Policy) error
	Update(ctx context.Context, p *Policy) error
	Delete(ctx context.Context, id ID) error
	Get(ctx context.Context, id ID) (*Policy, error)
	ListByTenant(ctx context.Context, tenantID string) ([]*Policy, error)
}
