package tfcontextapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/tfcontext"
)

var ErrNotFound = errors.New("context not found")

type Service struct {
	Contexts tfcontext.Repository
	Now      func() time.Time
	NewID    func() string
}

func (s *Service) List(ctx context.Context, tenantID string) ([]*tfcontext.Context, error) {
	return s.Contexts.ListByTenant(ctx, tenantID)
}

func (s *Service) Get(ctx context.Context, tenantID string, id tfcontext.ID) (*tfcontext.Context, error) {
	c, err := s.Contexts.Get(ctx, id, false)
	if err != nil || c == nil {
		return nil, err
	}
	if c.Snapshot().TenantID != tenantID {
		return nil, nil
	}
	return c, nil
}

func (s *Service) Create(ctx context.Context, tenantID, name string) (*tfcontext.Context, error) {
	c, err := tfcontext.New(tfcontext.ID(s.NewID()), tenantID, name, s.Now())
	if err != nil {
		return nil, err
	}
	if err := s.Contexts.Create(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Service) Delete(ctx context.Context, tenantID string, id tfcontext.ID) error {
	c, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound
	}
	return s.Contexts.Delete(ctx, id)
}

func (s *Service) UpsertEntry(ctx context.Context, tenantID string, contextID tfcontext.ID, kind tfcontext.EntryKind, key, value string) error {
	c, err := s.Get(ctx, tenantID, contextID)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound
	}
	entry, err := tfcontext.NewEntry(s.NewID(), kind, key, value, s.Now())
	if err != nil {
		return err
	}
	return s.Contexts.UpsertEntry(ctx, contextID, entry)
}

func (s *Service) DeleteEntry(ctx context.Context, tenantID string, contextID tfcontext.ID, entryID string) error {
	c, err := s.Get(ctx, tenantID, contextID)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound
	}
	return s.Contexts.DeleteEntry(ctx, contextID, entryID)
}

func (s *Service) SetProjectContexts(ctx context.Context, tenantID string, projectID project.ID, contextIDs []tfcontext.ID) error {
	for _, cid := range contextIDs {
		if cid == "" {
			continue
		}
		c, err := s.Get(ctx, tenantID, cid)
		if err != nil {
			return err
		}
		if c == nil {
			return fmt.Errorf("context %s not found", cid)
		}
	}
	return s.Contexts.SetProjectContexts(ctx, projectID, contextIDs)
}

func (s *Service) ListByProject(ctx context.Context, projectID project.ID) ([]*tfcontext.Context, error) {
	return s.Contexts.ListByProject(ctx, projectID, false)
}

func (s *Service) ResolveForProject(ctx context.Context, projectID project.ID) (map[string]string, map[string][]byte, error) {
	contexts, err := s.Contexts.ListByProject(ctx, projectID, true)
	if err != nil {
		return nil, nil, err
	}
	env, files := tfcontext.Resolve(contexts)
	return env, files, nil
}
