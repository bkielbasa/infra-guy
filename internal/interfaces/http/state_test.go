package httpapi

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	projectapp "infra-guy/internal/application/project"
	stateapp "infra-guy/internal/application/state"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/state"
)

type memProjects struct {
	items map[project.ID]*project.Project
	err   error
}

func (m *memProjects) Save(_ context.Context, p *project.Project) error {
	m.items[p.Snapshot().ID] = project.Restore(p.Snapshot())
	return nil
}
func (m *memProjects) Get(_ context.Context, id project.ID) (*project.Project, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.items[id], nil
}
func (m *memProjects) List(context.Context, string) ([]*project.Project, error) {
	out := make([]*project.Project, 0, len(m.items))
	for _, p := range m.items {
		out = append(out, p)
	}
	return out, nil
}
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

func testProject(t *testing.T) *project.Project {
	t.Helper()
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New("p1", "infra", repo, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	p.SetStateToken("tok")
	if err := p.SetPlacement("ten", 1); err != nil {
		t.Fatal(err)
	}
	return p
}

func stateMux(t *testing.T, projects *memProjects, states *memStates) http.Handler {
	t.Helper()
	svc := &stateapp.Service{Projects: projects, States: states, Now: time.Now}
	projSvc := &projectapp.Service{Projects: projects, States: svc, Now: time.Now}
	return New(svc, nil, projSvc)
}

func doState(t *testing.T, h http.Handler, method, path, user, pass string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestStateHTTP(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	states := &memStates{items: map[string]*state.State{}}
	h := stateMux(t, projects, states)

	rr := doState(t, h, http.MethodGet, "/api/projects/p1/state", "not-terraform", "tok", nil)
	if rr.Code != 401 {
		t.Fatalf("wrong user %d", rr.Code)
	}

	rr = doState(t, h, http.MethodGet, "/api/projects/p1/state", "terraform", "nope", nil)
	if rr.Code != 401 {
		t.Fatalf("wrong token %d", rr.Code)
	}

	rr = doState(t, h, http.MethodGet, "/api/projects/p1/state", "", "", nil)
	if rr.Code != 401 {
		t.Fatalf("missing auth %d", rr.Code)
	}

	rr = doState(t, h, http.MethodGet, "/api/projects/p1/state", "terraform", "tok", nil)
	if rr.Code != 404 {
		t.Fatalf("empty %d", rr.Code)
	}

	body := []byte(`{"serial":3}`)
	rr = doState(t, h, http.MethodPost, "/api/projects/p1/state", "terraform", "tok", body)
	if rr.Code != 200 {
		t.Fatalf("put %d %s", rr.Code, rr.Body.Bytes())
	}
	st, _ := states.Load(context.Background(), "p1")
	if hist := st.Snapshot().History; len(hist) != 1 || hist[0].RunID != "" {
		t.Fatalf("runID %#v", st.Snapshot().History)
	}

	rr = doState(t, h, http.MethodGet, "/api/projects/p1/state", "terraform", "tok", nil)
	sum := md5.Sum(body)
	wantMD5 := base64.StdEncoding.EncodeToString(sum[:])
	if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), body) || rr.Header().Get("Content-MD5") != wantMD5 {
		t.Fatalf("get %d %s md5 %s", rr.Code, rr.Body.Bytes(), rr.Header().Get("Content-MD5"))
	}

	lock := []byte(`{"ID":"L1"}`)
	rr = doState(t, h, "LOCK", "/api/projects/p1/state/lock", "terraform", "tok", lock)
	if rr.Code != 200 {
		t.Fatalf("lock %d", rr.Code)
	}
	rr = doState(t, h, "LOCK", "/api/projects/p1/state/lock", "terraform", "tok", []byte(`{"ID":"L2"}`))
	if rr.Code != 409 || rr.Body.String() != `{"ID":"L1"}` {
		t.Fatalf("conflict %d %s", rr.Code, rr.Body.Bytes())
	}

	rr = doState(t, h, http.MethodPost, "/api/projects/p1/state?ID=other", "terraform", "tok", []byte(`{"serial":9}`))
	if rr.Code != 409 {
		t.Fatalf("put mismatch %d", rr.Code)
	}
	rr = doState(t, h, http.MethodGet, "/api/projects/p1/state", "terraform", "tok", nil)
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"serial":3`)) {
		t.Fatalf("state changed: %s", rr.Body.Bytes())
	}

	rr = doState(t, h, http.MethodPost, "/api/projects/p1/state?ID=L1", "terraform", "tok", []byte(`{"serial":4}`))
	if rr.Code != 200 {
		t.Fatalf("put locked %d", rr.Code)
	}

	rr = doState(t, h, http.MethodPost, "/api/projects/p1/state?run=run-9&ID=L1", "terraform", "tok", []byte(`{"serial":5}`))
	if rr.Code != 200 {
		t.Fatalf("put with run %d", rr.Code)
	}
	st, _ = states.Load(context.Background(), "p1")
	if hist := st.Snapshot().History; len(hist) != 3 || hist[2].RunID != "run-9" {
		t.Fatalf("run query not recorded %#v", hist)
	}

	rr = doState(t, h, "UNLOCK", "/api/projects/p1/state/lock", "terraform", "tok", []byte(`{"ID":"L1"}`))
	if rr.Code != 200 {
		t.Fatalf("unlock %d", rr.Code)
	}

	rr = doState(t, h, http.MethodPost, "/api/projects/p1/state/lock", "terraform", "tok", []byte(`{"Who":"x"}`))
	if rr.Code != 500 {
		t.Fatalf("post lock without ID should call Lock %d", rr.Code)
	}
	rr = doState(t, h, "LOCK", "/api/projects/p1/state/lock", "terraform", "tok", lock)
	if rr.Code != 200 {
		t.Fatalf("relock %d", rr.Code)
	}
	rr = doState(t, h, http.MethodPost, "/api/projects/p1/state/lock", "terraform", "tok", lock)
	if rr.Code != 200 {
		t.Fatalf("post unlock %d", rr.Code)
	}
	rr = doState(t, h, "LOCK", "/api/projects/p1/state/lock", "terraform", "tok", lock)
	if rr.Code != 200 {
		t.Fatalf("lock after post unlock %d", rr.Code)
	}

	boom := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}, err: errors.New("boom")}
	h = stateMux(t, boom, states)
	rr = doState(t, h, http.MethodGet, "/api/projects/p1/state", "terraform", "tok", nil)
	if rr.Code != 500 {
		t.Fatalf("service error %d", rr.Code)
	}
}
