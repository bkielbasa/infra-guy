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
func (m *memProjects) List(context.Context) ([]*project.Project, error) { return nil, nil }
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
	return state.New(projectID), nil
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
