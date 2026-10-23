package tfcontextapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/tfcontext"
)

type fakeSealer struct{}

func (fakeSealer) Seal(plaintext []byte) ([]byte, error) {
	return append([]byte("enc:"), plaintext...), nil
}

func (fakeSealer) Open(ciphertext []byte) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, []byte("enc:")) {
		return nil, errors.New("invalid ciphertext")
	}
	return bytes.TrimPrefix(ciphertext, []byte("enc:")), nil
}

type memRepo struct {
	items    map[tfcontext.ID]*tfcontext.Context
	attached map[project.ID][]tfcontext.ID
}

func newMemRepo() *memRepo {
	return &memRepo{
		items:    map[tfcontext.ID]*tfcontext.Context{},
		attached: map[project.ID][]tfcontext.ID{},
	}
}

func (m *memRepo) Create(_ context.Context, c *tfcontext.Context) error {
	m.items[c.Snapshot().ID] = tfcontext.Restore(c.Snapshot())
	return nil
}

func (m *memRepo) Get(_ context.Context, id tfcontext.ID) (*tfcontext.Context, error) {
	c, ok := m.items[id]
	if !ok {
		return nil, nil
	}
	return tfcontext.Restore(c.Snapshot()), nil
}

func (m *memRepo) ListByTenant(_ context.Context, tenantID string) ([]*tfcontext.Context, error) {
	var out []*tfcontext.Context
	for _, c := range m.items {
		if c.Snapshot().TenantID == tenantID {
			cp, _ := m.Get(context.Background(), c.Snapshot().ID)
			out = append(out, cp)
		}
	}
	return out, nil
}

func (m *memRepo) Delete(_ context.Context, id tfcontext.ID) error {
	delete(m.items, id)
	return nil
}

func (m *memRepo) UpsertEntry(_ context.Context, contextID tfcontext.ID, entry tfcontext.Entry) error {
	c := m.items[contextID]
	s := c.Snapshot()
	replaced := false
	for i := range s.Entries {
		if s.Entries[i].Kind == entry.Kind && s.Entries[i].Key == entry.Key {
			s.Entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		s.Entries = append(s.Entries, entry)
	}
	m.items[contextID] = tfcontext.Restore(s)
	return nil
}

func (m *memRepo) DeleteEntry(_ context.Context, contextID tfcontext.ID, entryID string) error {
	c := m.items[contextID]
	s := c.Snapshot()
	var kept []tfcontext.Entry
	for _, e := range s.Entries {
		if e.ID != entryID {
			kept = append(kept, e)
		}
	}
	s.Entries = kept
	m.items[contextID] = tfcontext.Restore(s)
	return nil
}

func (m *memRepo) SetProjectContexts(_ context.Context, projectID project.ID, contextIDs []tfcontext.ID) error {
	m.attached[projectID] = append([]tfcontext.ID(nil), contextIDs...)
	return nil
}

func (m *memRepo) ListByProject(_ context.Context, projectID project.ID) ([]*tfcontext.Context, error) {
	var out []*tfcontext.Context
	for _, id := range m.attached[projectID] {
		c, _ := m.Get(context.Background(), id)
		if c != nil {
			out = append(out, c)
		}
	}
	return out, nil
}

func TestServiceTenantIsolationAndResolution(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	seq := 0
	svc := &Service{
		Contexts: repo,
		Sealer:   fakeSealer{},
		Now:      func() time.Time { return time.Unix(100, 0).UTC() },
		NewID: func() string {
			seq++
			return fmt.Sprintf("id-%d", seq)
		},
	}

	c1, err := svc.Create(ctx, "ten-1", "aws-creds")
	if err != nil {
		t.Fatal(err)
	}
	// Other tenant cannot access or modify c1
	if got, err := svc.Get(ctx, "ten-2", c1.Snapshot().ID); err != nil || got != nil {
		t.Fatalf("cross-tenant Get returned %v, %v", got, err)
	}
	if err := svc.UpsertEntry(ctx, "ten-2", c1.Snapshot().ID, tfcontext.KindPlainEnv, "FOO", "bar"); err == nil {
		t.Fatal("cross-tenant UpsertEntry succeeded")
	}

	// Valid entries on ten-1
	if err := svc.UpsertEntry(ctx, "ten-1", c1.Snapshot().ID, tfcontext.KindSecretEnv, "AWS_SECRET_ACCESS_KEY", "sec-123"); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpsertEntry(ctx, "ten-1", c1.Snapshot().ID, tfcontext.KindSecretFile, "~/.kube/config", "k8s-cfg"); err != nil {
		t.Fatal(err)
	}

	// Attaching context from another tenant fails
	if err := svc.SetProjectContexts(ctx, "ten-2", "proj-1", []tfcontext.ID{c1.Snapshot().ID}); err == nil {
		t.Fatal("cross-tenant SetProjectContexts succeeded")
	}
	if err := svc.SetProjectContexts(ctx, "ten-1", "proj-1", []tfcontext.ID{c1.Snapshot().ID}); err != nil {
		t.Fatal(err)
	}

	env, files, err := svc.ResolveForProject(ctx, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if env["AWS_SECRET_ACCESS_KEY"] != "sec-123" || string(files[".kube/config"]) != "k8s-cfg" {
		t.Fatalf("unexpected resolved env=%v files=%v", env, files)
	}
}

func TestServiceCRUDAndEntryOperations(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	seq := 0
	svc := &Service{
		Contexts: repo,
		Sealer:   fakeSealer{},
		Now:      func() time.Time { return time.Unix(100, 0).UTC() },
		NewID: func() string {
			seq++
			return fmt.Sprintf("id-%d", seq)
		},
	}

	c1, err := svc.Create(ctx, "ten-1", "ctx-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpsertEntry(ctx, "ten-1", c1.Snapshot().ID, tfcontext.KindPlainEnv, "APP_ENV", "prod"); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpsertEntry(ctx, "ten-1", c1.Snapshot().ID, tfcontext.KindSecretEnv, "SECRET_KEY", "super-secret"); err != nil {
		t.Fatal(err)
	}

	list, err := svc.List(ctx, "ten-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 context for ten-1, got %d, err=%v", len(list), err)
	}

	got, err := svc.Get(ctx, "ten-1", c1.Snapshot().ID)
	if err != nil || got == nil {
		t.Fatalf("expected context, got %v, err=%v", got, err)
	}
	entries := got.Snapshot().Entries
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Cross-tenant DeleteEntry and Delete fail with ErrNotFound
	if err := svc.DeleteEntry(ctx, "ten-2", c1.Snapshot().ID, entries[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on cross-tenant DeleteEntry, got %v", err)
	}
	if err := svc.Delete(ctx, "ten-2", c1.Snapshot().ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on cross-tenant Delete, got %v", err)
	}

	// Same-tenant DeleteEntry succeeds
	if err := svc.DeleteEntry(ctx, "ten-1", c1.Snapshot().ID, entries[0].ID); err != nil {
		t.Fatalf("DeleteEntry failed: %v", err)
	}
	gotAfter, _ := svc.Get(ctx, "ten-1", c1.Snapshot().ID)
	if len(gotAfter.Snapshot().Entries) != 1 {
		t.Fatalf("expected 1 entry after delete, got %d", len(gotAfter.Snapshot().Entries))
	}

	// ListByProject masks secrets (sealed in UpsertEntry)
	if err := svc.SetProjectContexts(ctx, "ten-1", "proj-1", []tfcontext.ID{"", c1.Snapshot().ID}); err != nil {
		t.Fatalf("SetProjectContexts failed: %v", err)
	}
	projCtxs, err := svc.ListByProject(ctx, "proj-1")
	if err != nil || len(projCtxs) != 1 {
		t.Fatalf("expected 1 project context, got %d, err=%v", len(projCtxs), err)
	}
	if projCtxs[0].Snapshot().Entries[0].Value != "" {
		t.Fatalf("expected secret value masked in ListByProject, got %q", projCtxs[0].Snapshot().Entries[0].Value)
	}

	// Same-tenant Delete succeeds
	if err := svc.Delete(ctx, "ten-1", c1.Snapshot().ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if gotDeleted, err := svc.Get(ctx, "ten-1", c1.Snapshot().ID); err != nil || gotDeleted != nil {
		t.Fatalf("expected nil after delete, got %v, err=%v", gotDeleted, err)
	}
}

func TestServiceSealsSecretsAndDeduplicatesProjectContexts(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	seq := 0
	svc := &Service{
		Contexts: repo,
		Sealer:   fakeSealer{},
		Now:      func() time.Time { return time.Unix(100, 0).UTC() },
		NewID: func() string {
			seq++
			return fmt.Sprintf("id-%d", seq)
		},
	}

	c1, err := svc.Create(ctx, "t-1", "aws")
	if err != nil {
		t.Fatal(err)
	}
	cid := c1.Snapshot().ID
	if err := svc.UpsertEntry(ctx, "t-1", cid, tfcontext.KindSecretEnv, "AWS_SECRET", "super-secret"); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpsertEntry(ctx, "t-1", cid, tfcontext.KindPlainEnv, "AWS_REGION", "eu-west-1"); err != nil {
		t.Fatal(err)
	}

	stored, err := svc.Get(ctx, "t-1", cid)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range stored.Snapshot().Entries {
		if e.Key == "AWS_SECRET" {
			if e.Value != "" {
				t.Fatalf("expected empty plaintext Value on stored secret, got %q", e.Value)
			}
			if string(e.Ciphertext) != "enc:super-secret" {
				t.Fatalf("expected sealed Ciphertext, got %q", string(e.Ciphertext))
			}
		}
	}

	if err := svc.SetProjectContexts(ctx, "t-1", "p-1", []tfcontext.ID{"", cid, cid, ""}); err != nil {
		t.Fatal(err)
	}
	if got := repo.attached["p-1"]; len(got) != 1 || got[0] != cid {
		t.Fatalf("expected deduplicated [%s], got %v", cid, got)
	}

	env, _, err := svc.ResolveForProject(ctx, "p-1")
	if err != nil {
		t.Fatal(err)
	}
	if env["AWS_SECRET"] != "super-secret" || env["AWS_REGION"] != "eu-west-1" {
		t.Fatalf("unexpected resolved env: %+v", env)
	}
}
