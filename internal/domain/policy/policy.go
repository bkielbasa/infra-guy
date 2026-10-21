package policy

import (
	"context"
	"errors"
	"strings"
	"time"
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
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type NewInput struct {
	ID          ID
	TenantID    string
	Name        string
	Description string
	Rego        string
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
	return &Policy{
		s: Snapshot{
			ID:          id,
			TenantID:    tenantID,
			Name:        name,
			Description: strings.TrimSpace(in.Description),
			Rego:        in.Rego,
			CreatedAt:   in.Now,
			UpdatedAt:   in.Now,
		},
	}, nil
}

func Rehydrate(s Snapshot) *Policy {
	return &Policy{s: s}
}

func Restore(s Snapshot) *Policy {
	return &Policy{s: s}
}

func (p *Policy) Snapshot() Snapshot {
	return p.s
}

func (p *Policy) ID() ID               { return p.s.ID }
func (p *Policy) TenantID() string     { return p.s.TenantID }
func (p *Policy) Name() string         { return p.s.Name }
func (p *Policy) Description() string  { return p.s.Description }
func (p *Policy) Rego() string         { return p.s.Rego }
func (p *Policy) CreatedAt() time.Time { return p.s.CreatedAt }
func (p *Policy) UpdatedAt() time.Time { return p.s.UpdatedAt }

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

type Repository interface {
	Insert(ctx context.Context, p *Policy) error
	Update(ctx context.Context, p *Policy) error
	Delete(ctx context.Context, id ID) error
	Get(ctx context.Context, id ID) (*Policy, error)
	ListByTenant(ctx context.Context, tenantID string) ([]*Policy, error)
}
