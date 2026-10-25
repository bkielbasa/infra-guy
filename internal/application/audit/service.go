package auditapp

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"infra-guy/internal/domain/audit"
)

type Service struct {
	Repo  audit.Repository
	Now   func() time.Time
	NewID func() string
	seq   atomic.Int64
}

type RecordInput struct {
	TenantID     string
	ActorID      string
	ActorLogin   string
	ActorRole    string
	Action       string
	ResourceType string
	ResourceID   string
	ResourceName string
	Summary      string
	Metadata     map[string]any
	IPAddress    string
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) newID() string {
	if s != nil && s.NewID != nil {
		return s.NewID()
	}
	n := s.seq.Add(1)
	return fmt.Sprintf("aud-%d-%d", s.now().UnixNano(), n)
}

func (s *Service) Record(ctx context.Context, in RecordInput) (audit.Snapshot, error) {
	if s == nil || s.Repo == nil {
		return audit.Snapshot{}, nil
	}
	tenantID := strings.TrimSpace(in.TenantID)
	if tenantID == "" {
		tenantID = "default"
	}
	actorLogin := strings.TrimSpace(in.ActorLogin)
	if actorLogin == "" {
		actorLogin = "system"
	}
	entry := audit.Snapshot{
		ID:           audit.ID(s.newID()),
		TenantID:     tenantID,
		ActorID:      strings.TrimSpace(in.ActorID),
		ActorLogin:   actorLogin,
		ActorRole:    strings.TrimSpace(in.ActorRole),
		Action:       strings.TrimSpace(in.Action),
		ResourceType: strings.TrimSpace(in.ResourceType),
		ResourceID:   strings.TrimSpace(in.ResourceID),
		ResourceName: strings.TrimSpace(in.ResourceName),
		Summary:      strings.TrimSpace(in.Summary),
		Metadata:     in.Metadata,
		IPAddress:    strings.TrimSpace(in.IPAddress),
		CreatedAt:    s.now().Truncate(time.Microsecond),
	}
	return s.Repo.Append(ctx, entry)
}

func (s *Service) List(ctx context.Context, tenantID string, filter audit.Filter) ([]audit.Snapshot, error) {
	if s == nil || s.Repo == nil {
		return nil, nil
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		tenantID = "default"
	}
	return s.Repo.ListByTenant(ctx, tenantID, filter)
}
