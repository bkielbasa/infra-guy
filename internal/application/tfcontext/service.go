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

type SecretSealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
}

type Service struct {
	Contexts tfcontext.Repository
	Sealer   SecretSealer
	Now      func() time.Time
	NewID    func() string
}

func (s *Service) List(ctx context.Context, tenantID string) ([]*tfcontext.Context, error) {
	return s.Contexts.ListByTenant(ctx, tenantID)
}

func (s *Service) Get(ctx context.Context, tenantID string, id tfcontext.ID) (*tfcontext.Context, error) {
	c, err := s.Contexts.Get(ctx, id)
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
	if entry.Secret && s.Sealer != nil {
		ct, err := s.Sealer.Seal([]byte(entry.Value))
		if err != nil {
			return fmt.Errorf("seal context entry %q: %w", entry.Key, err)
		}
		entry.Ciphertext = ct
		entry.Value = ""
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
	seen := make(map[tfcontext.ID]bool, len(contextIDs))
	deduped := make([]tfcontext.ID, 0, len(contextIDs))
	for _, cid := range contextIDs {
		if cid == "" || seen[cid] {
			continue
		}
		seen[cid] = true
		c, err := s.Get(ctx, tenantID, cid)
		if err != nil {
			return err
		}
		if c == nil {
			return fmt.Errorf("context %s not found", cid)
		}
		deduped = append(deduped, cid)
	}
	return s.Contexts.SetProjectContexts(ctx, projectID, deduped)
}

func (s *Service) ListByProject(ctx context.Context, projectID project.ID) ([]*tfcontext.Context, error) {
	return s.Contexts.ListByProject(ctx, projectID)
}

func (s *Service) ResolveForProject(ctx context.Context, projectID project.ID) (map[string]string, map[string][]byte, error) {
	contexts, err := s.Contexts.ListByProject(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range contexts {
		if c == nil {
			continue
		}
		entries := c.Snapshot().Entries
		for i := range entries {
			if entries[i].Secret && len(entries[i].Ciphertext) > 0 && s.Sealer != nil {
				pt, err := s.Sealer.Open(entries[i].Ciphertext)
				if err != nil {
					return nil, nil, fmt.Errorf("decrypt context entry %q: %w", entries[i].Key, err)
				}
				entries[i].Value = string(pt)
			}
		}
		c.SetEntries(entries)
	}
	env, files := tfcontext.Resolve(contexts)
	return env, files, nil
}
