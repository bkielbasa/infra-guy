package stateapp

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"testing"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/state"
)

type memProjects struct {
	items map[project.ID]*project.Project
}

func (m *memProjects) Save(_ context.Context, p *project.Project) error {
	m.items[p.Snapshot().ID] = project.Restore(p.Snapshot())
	return nil
}
func (m *memProjects) Get(_ context.Context, id project.ID) (*project.Project, error) {
	return m.items[id], nil
}
func (m *memProjects) List(context.Context, string) ([]*project.Project, error) { return nil, nil }
func (m *memProjects) FindByRepo(context.Context, string, int64) ([]*project.Project, error) {
	return nil, nil
}
func (m *memProjects) SetLocked(_ context.Context, id project.ID, locked bool) error {
	if p := m.items[id]; p != nil {
		s := p.Snapshot()
		s.Locked = locked
		m.items[id] = project.Restore(s)
	}
	return nil
}
func (m *memProjects) ReleaseActiveRun(_ context.Context, id project.ID, runID string) error {
	if p := m.items[id]; p != nil {
		_ = p.ReleaseActiveRun(runID)
	}
	return nil
}
func (m *memProjects) Delete(_ context.Context, id project.ID) error {
	delete(m.items, id)
	return nil
}

type memStates struct{ items map[string]*state.State }

func (m *memStates) Load(_ context.Context, projectID string) (*state.State, error) {
	if st, ok := m.items[projectID]; ok {
		return state.Restore(st.Snapshot()), nil
	}
	return nil, nil
}

func (m *memStates) Save(_ context.Context, s *state.State) error {
	m.items[s.Snapshot().ProjectID] = state.Restore(s.Snapshot())
	return nil
}

func TestStateHTTPOutcomes(t *testing.T) {
	ctx := context.Background()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	p.SetStateToken("tok")
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	states := &memStates{items: map[string]*state.State{}}
	svc := &Service{Projects: projects, States: states, Now: time.Now}
	if got, _ := svc.Get(ctx, "p1", "nope"); got.Code != 401 {
		t.Fatalf("auth %d", got.Code)
	}
	if got, _ := svc.Get(ctx, "p1", "tok"); got.Code != 404 {
		t.Fatalf("empty %d", got.Code)
	}
	body := []byte(`{"serial":3}`)
	if got, err := svc.Put(ctx, "p1", "tok", "", body, "run-1"); err != nil || got.Code != 200 {
		t.Fatalf("put %v %#v", err, got)
	}
	got, _ := svc.Get(ctx, "p1", "tok")
	sum := md5.Sum(body)
	want := base64.StdEncoding.EncodeToString(sum[:])
	if got.Code != 200 || string(got.Body) != string(body) || got.MD5 != want {
		t.Fatalf("%d %s md5 %s", got.Code, got.Body, got.MD5)
	}
	if got, _ := svc.Lock(ctx, "p1", "tok", []byte(`{"ID":"L1"}`)); got.Code != 200 {
		t.Fatal(got.Code)
	}
	got, _ = svc.Lock(ctx, "p1", "tok", []byte(`{"ID":"L2"}`))
	if got.Code != 409 || string(got.Body) != `{"ID":"L1"}` {
		t.Fatalf("conflict %d %s", got.Code, got.Body)
	}
	got, _ = svc.Put(ctx, "p1", "tok", "other", []byte(`{"serial":9}`), "run-1")
	if got.Code != 409 {
		t.Fatal(got.Code)
	}
	got, _ = svc.Get(ctx, "p1", "tok")
	if !bytes.Contains(got.Body, []byte(`"serial":3`)) {
		t.Fatalf("state changed: %s", got.Body)
	}
}

func TestOutputsChangedAndForceUnlock(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(100, 0).UTC()
	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New("p1", "infra", repo, now)
	p.SetStateToken("tok")
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	states := &memStates{items: map[string]*state.State{}}
	svc := &Service{Projects: projects, States: states, Now: func() time.Time { return now }}

	// Empty state returns nil without error
	outs, err := svc.Outputs(ctx, "p1")
	if err != nil || outs != nil {
		t.Fatalf("Outputs on missing state: %v, %v", outs, err)
	}
	changed, err := svc.ChangedOutputsSincePrevious(ctx, "p1")
	if err != nil || changed != nil {
		t.Fatalf("ChangedOutputsSincePrevious on missing state: %v, %v", changed, err)
	}
	if err := svc.ForceUnlock(ctx, "p1"); err != nil {
		t.Fatalf("ForceUnlock on missing state: %v", err)
	}

	// Put state v1 and v2
	v1 := []byte(`{"version":4,"serial":1,"outputs":{"vpc_id":{"value":"vpc-1","type":"string"}}}`)
	v2 := []byte(`{"version":4,"serial":2,"outputs":{"vpc_id":{"value":"vpc-2","type":"string"}}}`)
	if _, err := svc.Put(ctx, "p1", "tok", "", v1, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Put(ctx, "p1", "tok", "", v2, "r2"); err != nil {
		t.Fatal(err)
	}

	outs, err = svc.Outputs(ctx, "p1")
	if err != nil || outs["vpc_id"].EnvString != "vpc-2" {
		t.Fatalf("got outputs %+v, err %v", outs, err)
	}
	changed, err = svc.ChangedOutputsSincePrevious(ctx, "p1")
	if err != nil || !changed["vpc_id"] {
		t.Fatalf("got changed %+v, err %v", changed, err)
	}

	// Lock then ForceUnlock
	if out, err := svc.Lock(ctx, "p1", "tok", []byte(`{"ID":"lock-1"}`)); err != nil || out.Code != 200 {
		t.Fatalf("lock: %+v, %v", out, err)
	}
	if err := svc.ForceUnlock(ctx, "p1"); err != nil {
		t.Fatalf("force unlock: %v", err)
	}
	st, _ := states.Load(ctx, "p1")
	if st.Snapshot().LockID != "" {
		t.Fatalf("expected LockID cleared, got %q", st.Snapshot().LockID)
	}
}

func TestMetadataAndImport(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(200, 0).UTC()
	states := &memStates{items: map[string]*state.State{}}
	svc := &Service{States: states, Now: func() time.Time { return now }}

	meta, err := svc.Metadata(ctx, "p1")
	if err != nil || meta.Exists {
		t.Fatalf("expected empty metadata, got %+v, err=%v", meta, err)
	}

	if err := svc.Import(ctx, "p1", []byte("not-json")); err != state.ErrInvalidState {
		t.Fatalf("expected ErrInvalidState, got %v", err)
	}

	raw := []byte(`{"version":4,"serial":12,"outputs":{"out1":{"value":"val1"}}}`)
	if err := svc.Import(ctx, "p1", raw); err != nil {
		t.Fatalf("Import failed: %v", err)
	}

	meta, err = svc.Metadata(ctx, "p1")
	if err != nil || !meta.Exists || meta.Serial == nil || *meta.Serial != 12 || meta.Size != len(raw) || !meta.UpdatedAt.Equal(now) {
		t.Fatalf("unexpected metadata after import: %+v, err=%v", meta, err)
	}
}


