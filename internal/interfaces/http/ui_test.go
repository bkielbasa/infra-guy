package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	stateapp "infra-guy/internal/application/state"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/state"
)

type memRuns struct{ items map[run.ID]*run.Run }

func (m *memRuns) InsertIfAbsent(_ context.Context, r *run.Run) (bool, error) {
	m.items[r.Snapshot().ID] = run.Restore(r.Snapshot())
	return true, nil
}
func (m *memRuns) Save(_ context.Context, r *run.Run) error {
	m.items[r.Snapshot().ID] = run.Restore(r.Snapshot())
	return nil
}
func (m *memRuns) Get(_ context.Context, id run.ID) (*run.Run, error) { return m.items[id], nil }
func (m *memRuns) ListByProject(_ context.Context, projectID run.ProjectID) ([]*run.Run, error) {
	var out []*run.Run
	for _, r := range m.items {
		if r.Snapshot().ProjectID == projectID {
			out = append(out, r)
		}
	}
	return out, nil
}

type nopSched struct{}

func (nopSched) Enqueue(context.Context, run.ID, run.StepName) error { return nil }

type stubTip struct {
	sha string
	err error
}

func (s stubTip) BranchTip(context.Context, string, string, string) (string, error) {
	return s.sha, s.err
}

type stubLookup struct{ message, author string }

func (s stubLookup) Lookup(context.Context, project.SourceRepo, string, string) (string, string, error) {
	return s.message, s.author, nil
}

func uiServices(projects *memProjects, runs *memRuns, states *memStates) (*projectapp.Service, *runapp.Service, *stateapp.Service) {
	return &projectapp.Service{Projects: projects, Now: time.Now, NewID: func() string { return "new-p" }},
		&runapp.Service{Projects: projects, Runs: runs, Schedule: nopSched{}, Now: time.Now, NewID: func() string { return "new-r" }},
		&stateapp.Service{Projects: projects, States: states, Now: time.Now}
}

func uiHandler(t *testing.T, projects *memProjects, runs *memRuns, states *memStates, user, pass string) http.Handler {
	t.Helper()
	proj, runsSvc, st := uiServices(projects, runs, states)
	return NewServer(ServerConfig{
		Projects: proj, Runs: runsSvc, States: st,
		Checkout: stubTip{sha: "abc1234"}, Lookup: stubLookup{},
		PublicBaseURL: "http://localhost:8080",
		BasicAuthUser: user, BasicAuthPassword: pass,
	})
}

func getUI(t *testing.T, h http.Handler, path, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func postUI(t *testing.T, h http.Handler, path, user, pass string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, body)
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func needsApprovalSnap(id run.ID) run.Snapshot {
	return run.Snapshot{
		ID: id, ProjectID: "p1", Trigger: run.TriggerPush, Status: run.StatusNeedsApproval,
		SHA: "abc1234def", Message: "please confirm",
		Steps: []run.Step{
			{Name: run.StepInit, Position: 1, Status: run.StepSucceeded},
			{Name: run.StepPlan, Position: 2, Status: run.StepSucceeded},
			{Name: run.StepApply, Position: 3, Status: run.StepPending},
		},
	}
}

func TestUI(t *testing.T) {
	p := testProject(t)
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	states := &memStates{items: map[string]*state.State{}}
	h := uiHandler(t, projects, runs, states, "", "")

	rr := getUI(t, h, "/", "", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "infra") {
		t.Fatalf("list %d %s", rr.Code, rr.Body.Bytes())
	}

	rr = getUI(t, h, "/projects/p1", "", "")
	page := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(page, "State managed by infra-guy") || !strings.Contains(page, "Tracked runs") {
		t.Fatalf("project %d %s", rr.Code, rr.Body.Bytes())
	}

	push := run.Restore(run.Snapshot{
		ID: "r-push", ProjectID: "p1", Trigger: run.TriggerPush, Status: run.StatusSucceeded,
		SHA: "abcdef1234567", Message: "remove init scripts",
		SummaryPresent: true, Add: 12, CreatedAt: time.Unix(1, 0).UTC(),
	})
	runs.items["r-push"] = push
	rr = getUI(t, h, "/projects/p1/tabs/tracked", "", "")
	tab := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(tab, "remove init scripts") || !strings.Contains(tab, "+12") {
		t.Fatalf("tracked %d %s", rr.Code, rr.Body.Bytes())
	}
	if strings.Contains(tab, `hx-trigger="every 2s"`) {
		t.Fatalf("terminal tab still polling: %s", rr.Body.Bytes())
	}

	need := run.Restore(needsApprovalSnap("r-need"))
	runs.items["r-need"] = need
	rr = getUI(t, h, "/runs/r-need", "", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Confirm") {
		t.Fatalf("run %d %s", rr.Code, rr.Body.Bytes())
	}

	authRuns := &memRuns{items: map[run.ID]*run.Run{"r-auth": run.Restore(needsApprovalSnap("r-auth"))}}
	authProjects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	authStates := &memStates{items: map[string]*state.State{}}
	_, authSvc, _ := uiServices(authProjects, authRuns, authStates)
	authH := NewServer(ServerConfig{
		Projects: &projectapp.Service{Projects: authProjects, Now: time.Now, NewID: func() string { return "new-p" }},
		Runs:     authSvc, States: &stateapp.Service{Projects: authProjects, States: authStates, Now: time.Now},
		Checkout: stubTip{sha: "abc1234"}, Lookup: stubLookup{},
		PublicBaseURL: "http://localhost:8080",
		BasicAuthUser: "bkilebasa", BasicAuthPassword: "pw",
	})
	rr = postUI(t, authH, "/runs/r-auth/confirm", "bkilebasa", "pw", nil)
	if rr.Code != http.StatusSeeOther && rr.Code != 200 {
		t.Fatalf("confirm auth %d %s", rr.Code, rr.Body.Bytes())
	}
	got, err := authSvc.Get(context.Background(), "r-auth")
	if err != nil || got == nil || got.Snapshot().ApprovedBy != "bkilebasa" {
		t.Fatalf("actor bkilebasa: err=%v approved=%v", err, got)
	}

	opRuns := &memRuns{items: map[run.ID]*run.Run{"r-op": run.Restore(needsApprovalSnap("r-op"))}}
	opProjects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	opH := uiHandler(t, opProjects, opRuns, &memStates{items: map[string]*state.State{}}, "", "")
	rr = postUI(t, opH, "/runs/r-op/confirm", "", "", nil)
	if rr.Code != http.StatusSeeOther && rr.Code != 200 {
		t.Fatalf("confirm operator %d %s", rr.Code, rr.Body.Bytes())
	}
	got, err = opRuns.Get(context.Background(), "r-op")
	if err != nil || got == nil || got.Snapshot().ApprovedBy != "operator" {
		t.Fatalf("actor operator: err=%v approved=%v", err, got)
	}
}
