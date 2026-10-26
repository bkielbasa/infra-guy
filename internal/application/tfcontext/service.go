package tfcontextapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/tfcontext"
)

var ErrNotFound = errors.New("context not found")

type SecretSealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
}

type AADSecretSealer interface {
	SecretSealer
	SealWithAAD(plaintext, aad []byte) ([]byte, error)
	OpenWithAAD(ciphertext, aad []byte) ([]byte, error)
}

type ExternalSecretResolver func(ctx context.Context, ref string) (string, error)

// EntryAAD constructs the deterministic AEAD Associated Data for a context entry.
func EntryAAD(contextID tfcontext.ID, kind tfcontext.EntryKind, key string) []byte {
	return []byte("context_entry:" + string(contextID) + ":" + string(kind) + ":" + key)
}

type Service struct {
	Contexts         tfcontext.Repository
	Sealer           SecretSealer
	ExternalResolver ExternalSecretResolver
	Now              func() time.Time
	NewID            func() string
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

func (s *Service) Create(ctx context.Context, tenantID, name string, tags ...map[string]string) (*tfcontext.Context, error) {
	c, err := tfcontext.New(tfcontext.ID(s.NewID()), tenantID, name, s.Now())
	if err != nil {
		return nil, err
	}
	if len(tags) > 0 && len(tags[0]) > 0 {
		if err := c.SetTags(tags[0]); err != nil {
			return nil, err
		}
	}
	if err := s.Contexts.Create(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Service) Update(ctx context.Context, tenantID string, id tfcontext.ID, name string, tags map[string]string) (*tfcontext.Context, error) {
	c, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound
	}
	if err := c.Update(name, tags, s.Now()); err != nil {
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
		aad := EntryAAD(contextID, entry.Kind, entry.Key)
		var ct []byte
		if aadSealer, ok := s.Sealer.(AADSecretSealer); ok {
			ct, err = aadSealer.SealWithAAD([]byte(entry.Value), aad)
		} else {
			ct, err = s.Sealer.Seal([]byte(entry.Value))
		}
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
		cs := c.Snapshot()
		entries := cs.Entries
		for i := range entries {
			if entries[i].Secret && len(entries[i].Ciphertext) > 0 && s.Sealer != nil {
				aad := EntryAAD(cs.ID, entries[i].Kind, entries[i].Key)
				var pt []byte
				if aadSealer, ok := s.Sealer.(AADSecretSealer); ok {
					pt, err = aadSealer.OpenWithAAD(entries[i].Ciphertext, aad)
				} else {
					pt, err = s.Sealer.Open(entries[i].Ciphertext)
				}
				if err != nil {
					return nil, nil, fmt.Errorf("decrypt context entry %q: %w", entries[i].Key, err)
				}
				entries[i].Value = string(pt)
			}
			if IsExternalSecretReference(entries[i].Value) {
				resolved, err := s.resolveExternalReference(ctx, entries[i].Value)
				if err != nil {
					return nil, nil, fmt.Errorf("resolve external secret for %q: %w", entries[i].Key, err)
				}
				entries[i].Value = resolved
			}
		}
		c.SetEntries(entries)
	}
	env, files := tfcontext.Resolve(contexts)
	return env, files, nil
}

// IsExternalSecretReference reports whether val is an external secret manager URI
// (env://, vault://, aws-sm://, ssm://).
func IsExternalSecretReference(val string) bool {
	return strings.HasPrefix(val, "env://") ||
		strings.HasPrefix(val, "vault://") ||
		strings.HasPrefix(val, "aws-sm://") ||
		strings.HasPrefix(val, "ssm://")
}

func (s *Service) resolveExternalReference(ctx context.Context, ref string) (string, error) {
	if s.ExternalResolver != nil {
		return s.ExternalResolver(ctx, ref)
	}
	switch {
	case strings.HasPrefix(ref, "env://"):
		name := strings.TrimPrefix(ref, "env://")
		if name == "" {
			return "", errors.New("empty env:// reference")
		}
		val, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %q is not set", name)
		}
		return val, nil
	case strings.HasPrefix(ref, "vault://"):
		return resolveVaultSecret(ctx, strings.TrimPrefix(ref, "vault://"))
	default:
		return "", fmt.Errorf("unsupported external secret reference scheme in %q (configure ExternalResolver)", ref)
	}
}

func resolveVaultSecret(ctx context.Context, ref string) (string, error) {
	vaultAddr := strings.TrimRight(os.Getenv("VAULT_ADDR"), "/")
	vaultToken := os.Getenv("VAULT_TOKEN")
	if vaultAddr == "" || vaultToken == "" {
		return "", errors.New("VAULT_ADDR and VAULT_TOKEN are required to resolve vault:// references")
	}
	path, field, _ := strings.Cut(ref, "#")
	path = strings.TrimLeft(path, "/")
	if path == "" {
		return "", errors.New("empty vault:// secret path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, vaultAddr+"/v1/"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Vault-Token", vaultToken)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vault HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	data := payload.Data
	if nested, ok := data["data"].(map[string]any); ok {
		data = nested
	}
	if field == "" {
		field = "value"
	}
	val, ok := data[field]
	if !ok {
		return "", fmt.Errorf("field %q not found in vault secret %q", field, path)
	}
	return fmt.Sprint(val), nil
}
