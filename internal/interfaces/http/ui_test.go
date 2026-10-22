package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	authapp "infra-guy/internal/application/auth"
	"infra-guy/internal/application/installation"
	policyapp "infra-guy/internal/application/policy"
	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	stateapp "infra-guy/internal/application/state"
	tfcontextapp "infra-guy/internal/application/tfcontext"
	"infra-guy/internal/domain/policy"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/state"
	"infra-guy/internal/domain/tenant"
	"infra-guy/internal/domain/tfcontext"
	"infra-guy/internal/domain/user"
	"infra-guy/internal/infrastructure/opa"
)

type memRuns struct {
	items     map[run.ID]*run.Run
	duplicate bool
	insertErr error
	saveErr   error
}

func (m *memRuns) InsertIfAbsent(_ context.Context, r *run.Run) (bool, error) {
	if m.insertErr != nil {
		return false, m.insertErr
	}
	if m.duplicate {
		return false, nil
	}
	m.items[r.Snapshot().ID] = run.Restore(r.Snapshot())
	return true, nil
}
func (m *memRuns) Save(_ context.Context, r *run.Run) error {
	if m.saveErr != nil {
		return m.saveErr
	}
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
	sha   string
	err   error
	token string
}

func (s *stubTip) BranchTip(ctx context.Context, cloneURL, token, branch string) (string, error) {
	s.token = token
	return s.sha, s.err
}

type stubLookup struct {
	message, author string
	token           string
}

func (s *stubLookup) Lookup(ctx context.Context, repo project.SourceRepo, token, sha string) (string, string, error) {
	s.token = token
	return s.message, s.author, nil
}

func uiServices(projects *memProjects, runs *memRuns, states *memStates) (*projectapp.Service, *runapp.Service, *stateapp.Service) {
	return &projectapp.Service{Projects: projects, Now: time.Now, NewID: func() string { return "new-p" }},
		&runapp.Service{Projects: projects, Runs: runs, Schedule: nopSched{}, Now: time.Now, NewID: func() string { return "new-r" }},
		&stateapp.Service{Projects: projects, States: states, Now: time.Now}
}

const uiSessionID = "sess"

type uiTokenSource struct {
	store *uiIdentity
	err   error
}

func (s uiTokenSource) InstallationToken(ctx context.Context, tenantID string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if s.store == nil {
		return "ghs_install", nil
	}
	in, err := s.store.ActiveInstallation(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if in == nil || !in.Active {
		return "", installation.ErrNotInstalled
	}
	return "ghs_install", nil
}

func uiHandler(t *testing.T, projects *memProjects, runs *memRuns, states *memStates, login string) http.Handler {
	t.Helper()
	if login == "" {
		login = "operator"
	}
	proj, runsSvc, st := uiServices(projects, runs, states)
	store := newUIIdentity(login, "ten")
	gh := &uiGitHub{
		slug: "infra-guy", userID: 4, login: login,
		membershipState: "active", membershipRole: "admin", membershipStatus: http.StatusOK,
	}
	return NewServer(ServerConfig{
		Projects: proj, Runs: runsSvc, States: st,
		Checkout: &stubTip{sha: "abc1234"}, Lookup: &stubLookup{},
		Tokens:        uiTokenSource{store: store},
		PublicBaseURL: "http://localhost:8080",
		Auth: &authapp.Service{
			Store: store,
			GitHub: gh,
			Now: time.Now, NewID: func() string { return "id-1" },
			PublicBaseURL: "http://localhost:8080", ClientID: "cid",
		},
		Installations: &installation.Service{
			Store: store,
			GitHub: gh,
			Now: time.Now, NewID: func() string { return "inst-1" },
		},
	})
}

func getUI(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: uiSessionID})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func postUI(t *testing.T, h http.Handler, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(&http.Cookie{Name: "session", Value: uiSessionID})
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

func listProject(t *testing.T) *project.Project {
	t.Helper()
	repo, err := project.ParseSourceRepo("bklebasa/infra")
	if err != nil {
		t.Fatal(err)
	}
	p, err := project.New("p1", "payments", repo, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	p.SetStateToken("tok")
	if err := p.SetPlacement("ten", 1); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUI(t *testing.T) {
	p := listProject(t)
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	states := &memStates{items: map[string]*state.State{}}
	h := uiHandler(t, projects, runs, states, "")

	rr := getUI(t, h, "/")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "payments") {
		t.Fatalf("list %d %s", rr.Code, rr.Body.Bytes())
	}

	rr = getUI(t, h, "/projects/p1")
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
	rr = getUI(t, h, "/projects/p1/tabs/tracked")
	tab := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(tab, "remove init scripts") || !strings.Contains(tab, "+12") {
		t.Fatalf("tracked %d %s", rr.Code, rr.Body.Bytes())
	}
	if strings.Contains(tab, `hx-trigger="every 2s"`) {
		t.Fatalf("terminal tab still polling: %s", rr.Body.Bytes())
	}

	need := run.Restore(needsApprovalSnap("r-need"))
	runs.items["r-need"] = need
	rr = getUI(t, h, "/runs/r-need")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Confirm") {
		t.Fatalf("run %d %s", rr.Code, rr.Body.Bytes())
	}

	authRuns := &memRuns{items: map[run.ID]*run.Run{"r-auth": run.Restore(needsApprovalSnap("r-auth"))}}
	authProjects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	authH := uiHandler(t, authProjects, authRuns, &memStates{items: map[string]*state.State{}}, "bkilebasa")
	rr = postUI(t, authH, "/runs/r-auth/confirm", nil)
	if rr.Code != http.StatusSeeOther && rr.Code != 200 {
		t.Fatalf("confirm auth %d %s", rr.Code, rr.Body.Bytes())
	}
	got, err := authRuns.Get(context.Background(), "r-auth")
	if err != nil || got == nil || got.Snapshot().ApprovedBy != "bkilebasa" {
		t.Fatalf("actor bkilebasa: err=%v approved=%v", err, got)
	}

	opRuns := &memRuns{items: map[run.ID]*run.Run{"r-op": run.Restore(needsApprovalSnap("r-op"))}}
	opProjects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	opH := uiHandler(t, opProjects, opRuns, &memStates{items: map[string]*state.State{}}, "")
	rr = postUI(t, opH, "/runs/r-op/confirm", nil)
	if rr.Code != http.StatusSeeOther && rr.Code != 200 {
		t.Fatalf("confirm operator %d %s", rr.Code, rr.Body.Bytes())
	}
	got, err = opRuns.Get(context.Background(), "r-op")
	if err != nil || got == nil || got.Snapshot().ApprovedBy != "operator" {
		t.Fatalf("actor operator: err=%v approved=%v", err, got)
	}
}

func TestRunPageStateUpdated(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	runs := &memRuns{items: map[run.ID]*run.Run{
		"r-need": run.Restore(needsApprovalSnap("r-need")),
	}}
	st := state.New("p1")
	if err := st.Put([]byte(`{"serial":1}`), "", "r-need", time.Date(2026, 3, 4, 15, 6, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	h := uiHandler(t, projects, runs, &memStates{items: map[string]*state.State{"p1": st}}, "")
	rr := getUI(t, h, "/runs/r-need")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "2026-03-04 15:06") {
		t.Fatalf("run state updated %d %s", rr.Code, rr.Body.Bytes())
	}
}

func TestTriggerWithoutInstallationReturns502(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": listProject(t)}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	states := &memStates{items: map[string]*state.State{}}
	proj, runsSvc, st := uiServices(projects, runs, states)
	store := newUIIdentity("operator", "ten")
	gh := &uiGitHub{
		slug: "infra-guy", userID: 4, login: "operator",
		membershipState: "active", membershipRole: "admin", membershipStatus: http.StatusOK,
	}
	tip := &stubTip{sha: "abc1234"}
	lookup := &stubLookup{message: "commit msg", author: "author"}
	h := NewServer(ServerConfig{
		Projects: proj, Runs: runsSvc, States: st,
		Checkout: tip, Lookup: lookup,
		Tokens:        uiTokenSource{err: errors.New("installation inactive")},
		PublicBaseURL: "http://localhost:8080",
		Auth: &authapp.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "id-1" },
			PublicBaseURL: "http://localhost:8080", ClientID: "cid",
		},
		Installations: &installation.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "inst-1" },
		},
	})

	rr := postUI(t, h, "/projects/p1/trigger", nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("trigger without installation returned code %d, want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "GitHub App is not installed") {
		t.Fatalf("expected body to contain 'GitHub App is not installed', got %q", rr.Body.String())
	}
	if len(runs.items) != 0 {
		t.Fatalf("expected no runs opened, got %d", len(runs.items))
	}
}

func TestTriggerPassesInstallationToken(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": listProject(t)}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	states := &memStates{items: map[string]*state.State{}}
	proj, runsSvc, st := uiServices(projects, runs, states)
	store := newUIIdentity("operator", "ten")
	gh := &uiGitHub{
		slug: "infra-guy", userID: 4, login: "operator",
		membershipState: "active", membershipRole: "admin", membershipStatus: http.StatusOK,
	}
	tip := &stubTip{sha: "abc1234"}
	lookup := &stubLookup{message: "commit msg", author: "author"}
	h := NewServer(ServerConfig{
		Projects: proj, Runs: runsSvc, States: st,
		Checkout: tip, Lookup: lookup,
		Tokens:        uiTokenSource{},
		PublicBaseURL: "http://localhost:8080",
		Auth: &authapp.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "id-1" },
			PublicBaseURL: "http://localhost:8080", ClientID: "cid",
		},
		Installations: &installation.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "inst-1" },
		},
	})

	rr := postUI(t, h, "/projects/p1/trigger", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("trigger returned code %d, want 303: %s", rr.Code, rr.Body.String())
	}
	if tip.token != "ghs_install" {
		t.Fatalf("tip.token = %q, want 'ghs_install'", tip.token)
	}
	if lookup.token != "ghs_install" {
		t.Fatalf("lookup.token = %q, want 'ghs_install'", lookup.token)
	}
}

func TestTriggerDuplicateRedirectsToProject(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	h := uiHandler(t, projects, &memRuns{items: map[run.ID]*run.Run{}, duplicate: true}, &memStates{items: map[string]*state.State{}}, "")
	rr := postUI(t, h, "/projects/p1/trigger", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("duplicate trigger %d %s", rr.Code, rr.Body.Bytes())
	}
	if loc := rr.Header().Get("Location"); loc != "/projects/p1" {
		t.Fatalf("duplicate location %q", loc)
	}
}

func TestTriggerOpenErrorIsServerError(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	h := uiHandler(t, projects, &memRuns{items: map[run.ID]*run.Run{}, insertErr: errors.New("open failed")}, &memStates{items: map[string]*state.State{}}, "")
	rr := postUI(t, h, "/projects/p1/trigger", nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("open error %d %s", rr.Code, rr.Body.Bytes())
	}
}

func TestProjectHeadlinePollsWhileRunActive(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	runs := &memRuns{items: map[run.ID]*run.Run{
		"r-need": run.Restore(needsApprovalSnap("r-need")),
	}}
	h := uiHandler(t, projects, runs, &memStates{items: map[string]*state.State{}}, "")

	page := getUI(t, h, "/projects/p1")
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, `hx-get="/projects/p1/headline"`) || !strings.Contains(body, `hx-trigger="every 2s"`) {
		t.Fatalf("project headline poll %d %s", page.Code, page.Body.Bytes())
	}

	frag := getUI(t, h, "/projects/p1/headline")
	live := frag.Body.String()
	if frag.Code != 200 || !strings.Contains(live, `hx-trigger="every 2s"`) || strings.Contains(live, `id="tab-panel"`) {
		t.Fatalf("active headline %d %s", frag.Code, frag.Body.Bytes())
	}

	runs.items["r-need"] = run.Restore(run.Snapshot{
		ID: "r-need", ProjectID: "p1", Trigger: run.TriggerPush, Status: run.StatusSucceeded, SHA: "abc1234def",
	})
	done := getUI(t, h, "/projects/p1/headline")
	if done.Code != 200 || strings.Contains(done.Body.String(), `hx-trigger="every 2s"`) || strings.Contains(done.Body.String(), `id="tab-panel"`) {
		t.Fatalf("terminal headline %d %s", done.Code, done.Body.Bytes())
	}
}

func TestConfirmRejectsCrossSitePost(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	runs := &memRuns{items: map[run.ID]*run.Run{"r-need": run.Restore(needsApprovalSnap("r-need"))}}
	h := uiHandler(t, projects, runs, &memStates{items: map[string]*state.State{}}, "")

	cross := httptest.NewRequest(http.MethodPost, "http://infra.example/runs/r-need/confirm", nil)
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	cross.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, cross)
	if rr.Code != http.StatusForbidden || runs.items["r-need"].Snapshot().ApprovedBy != "" {
		t.Fatalf("cross-site confirm %d approved=%q", rr.Code, runs.items["r-need"].Snapshot().ApprovedBy)
	}

	same := httptest.NewRequest(http.MethodPost, "http://infra.example/runs/r-need/confirm", nil)
	same.Header.Set("Sec-Fetch-Site", "same-origin")
	same.Header.Set("Origin", "http://infra.example")
	same.AddCookie(&http.Cookie{Name: "session", Value: uiSessionID})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, same)
	if rr.Code != http.StatusSeeOther || runs.items["r-need"].Snapshot().ApprovedBy != "operator" {
		t.Fatalf("same-origin confirm %d %s", rr.Code, rr.Body.Bytes())
	}
}

func TestStateAPINotCrossOriginChecked(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	h := uiHandler(t, projects, &memRuns{items: map[run.ID]*run.Run{}}, &memStates{items: map[string]*state.State{}}, "")
	req := httptest.NewRequest(http.MethodPost, "/api/projects/p1/state", strings.NewReader(`{"serial":1}`))
	req.SetBasicAuth("terraform", "tok")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("state api %d", rr.Code)
	}
}

func TestConfirmActiveConflictIs409(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	runs := &memRuns{items: map[run.ID]*run.Run{"r-need": run.Restore(needsApprovalSnap("r-need"))}, saveErr: run.ErrActiveConflict}
	h := uiHandler(t, projects, runs, &memStates{items: map[string]*state.State{}}, "")
	rr := postUI(t, h, "/runs/r-need/confirm", nil)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "must finish") {
		t.Fatalf("conflict confirm %d %s", rr.Code, rr.Body.Bytes())
	}
}

func TestTerminalPanelsDoNotRefetch(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	runs := &memRuns{items: map[run.ID]*run.Run{"r-done": run.Restore(run.Snapshot{
		ID: "r-done", ProjectID: "p1", Trigger: run.TriggerPush, Status: run.StatusSucceeded, SHA: "abc1234def",
		Steps: []run.Step{{Name: run.StepInit, Position: 1, Status: run.StepSucceeded, Logs: "init ok"}},
	})}}
	h := uiHandler(t, projects, runs, &memStates{items: map[string]*state.State{}}, "")
	for _, path := range []string{
		"/runs/r-done/history", "/runs/r-done/headline", "/projects/p1/headline",
		"/projects/p1/tabs/tracked", "/projects/p1/tabs/pulls", "/projects/p1/tabs/state",
	} {
		body := getUI(t, h, path).Body.String()
		if strings.Contains(body, `hx-get="`+path+`"`) || strings.Contains(body, "hx-trigger") {
			t.Fatalf("terminal %s still refetches itself: %s", path, body)
		}
	}
	page := getUI(t, h, "/runs/r-done").Body.String()
	if strings.Contains(page, `hx-trigger`) || strings.Contains(page, `<div id="run-panel" hx-get`) {
		t.Fatalf("terminal run page still refetches: %s", page)
	}
}

func TestRunHeadlinePollsWhileActive(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	need := needsApprovalSnap("r-need")
	need.SummaryPresent = true
	need.Add = 12
	runs := &memRuns{items: map[run.ID]*run.Run{"r-need": run.Restore(need)}}
	h := uiHandler(t, projects, runs, &memStates{items: map[string]*state.State{}}, "")

	page := getUI(t, h, "/runs/r-need")
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, `hx-get="/runs/r-need/headline"`) || !strings.Contains(body, "Changes +12") {
		t.Fatalf("run headline poll %d %s", page.Code, page.Body.Bytes())
	}

	frag := getUI(t, h, "/runs/r-need/headline")
	live := frag.Body.String()
	if frag.Code != 200 || !strings.Contains(live, `hx-trigger="every 2s"`) || !strings.Contains(live, "Changes +12") || strings.Contains(live, `id="run-panel"`) {
		t.Fatalf("active run headline %d %s", frag.Code, frag.Body.Bytes())
	}

	runs.items["r-need"] = run.Restore(run.Snapshot{
		ID: "r-need", ProjectID: "p1", Trigger: run.TriggerPush, Status: run.StatusSucceeded,
		SHA: "abc1234def", SummaryPresent: true, Add: 12,
	})
	done := getUI(t, h, "/runs/r-need/headline")
	stopped := done.Body.String()
	if done.Code != 200 || strings.Contains(stopped, `hx-trigger="every 2s"`) || !strings.Contains(stopped, "Changes +12") || strings.Contains(stopped, `id="run-panel"`) {
		t.Fatalf("terminal run headline %d %s", done.Code, done.Body.Bytes())
	}
}

func TestConfirmHidesOtherTenantRun(t *testing.T) {
	own := testProject(t)
	repo, err := project.ParseSourceRepo("other/repo")
	if err != nil {
		t.Fatal(err)
	}
	other, err := project.New("p2", "other", repo, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SetPlacement("ten-other", 2); err != nil {
		t.Fatal(err)
	}
	snap := needsApprovalSnap("r-other")
	snap.ProjectID = "p2"
	runs := &memRuns{items: map[run.ID]*run.Run{"r-other": run.Restore(snap)}}
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": own, "p2": other}}
	h := uiHandler(t, projects, runs, &memStates{items: map[string]*state.State{}}, "")
	rr := postUI(t, h, "/runs/r-other/confirm", nil)
	if rr.Code != http.StatusNotFound || runs.items["r-other"].Snapshot().ApprovedBy != "" {
		t.Fatalf("cross-tenant confirm %d approved=%q", rr.Code, runs.items["r-other"].Snapshot().ApprovedBy)
	}
}

func TestGetProjectHidesOtherTenant(t *testing.T) {
	own := listProject(t)
	repo, err := project.ParseSourceRepo("other/repo")
	if err != nil {
		t.Fatal(err)
	}
	other, err := project.New("p2", "other", repo, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SetPlacement("ten-other", 2); err != nil {
		t.Fatal(err)
	}
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": own, "p2": other}}
	h := uiHandler(t, projects, &memRuns{items: map[run.ID]*run.Run{}}, &memStates{items: map[string]*state.State{}}, "")
	rr := getUI(t, h, "/projects/p2")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("other tenant %d %s", rr.Code, rr.Body.Bytes())
	}
	rr = getUI(t, h, "/projects/p1")
	if rr.Code != http.StatusOK {
		t.Fatalf("own tenant %d %s", rr.Code, rr.Body.Bytes())
	}
}

func TestCancelledHistoryNamesActor(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	runs := &memRuns{items: map[run.ID]*run.Run{"r-c": run.Restore(run.Snapshot{
		ID: "r-c", ProjectID: "p1", Trigger: run.TriggerManual, Status: run.StatusCancelled,
		SHA: "abc1234", ApprovedBy: "ada",
	})}}
	h := uiHandler(t, projects, runs, &memStates{items: map[string]*state.State{}}, "")
	body := getUI(t, h, "/runs/r-c/history").Body.String()
	if !strings.Contains(body, "Cancelled by ada") || strings.Contains(body, "Confirmed by") {
		t.Fatalf("history %s", body)
	}
}

func TestUIRedirectsToLoginWithoutSession(t *testing.T) {
	h := uiHandler(t, &memProjects{items: map[project.ID]*project.Project{}}, &memRuns{items: map[run.ID]*run.Run{}}, &memStates{items: map[string]*state.State{}}, "")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/github/login" {
		t.Fatalf("login redirect %d %s", rr.Code, rr.Header().Get("Location"))
	}
	staticReq := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
	staticRR := httptest.NewRecorder()
	h.ServeHTTP(staticRR, staticReq)
	if staticRR.Code != http.StatusOK {
		t.Fatalf("static %d", staticRR.Code)
	}
}

func TestHomeWithoutTenant(t *testing.T) {
	store := newUIIdentity("ada", "")
	gh := &uiGitHub{slug: "infra-guy", userID: 4, login: "ada", membershipState: "active", membershipRole: "admin", membershipStatus: http.StatusOK}
	h := uiWithAuth(t, &memProjects{items: map[project.ID]*project.Project{}}, store, gh)
	rr := getUI(t, h, "/")
	body := rr.Body.String()
	if rr.Code != http.StatusOK || !strings.Contains(body, "Install GitHub App") || !strings.Contains(body, "https://github.com/apps/infra-guy/installations/new") {
		t.Fatalf("install %d %s", rr.Code, body)
	}

	store.installations = []tenant.Installation{
		{ID: "i1", TenantID: "ten-1", AccountLogin: "acme", AccountType: "organization", GitHubInstallationID: 9, Active: true},
		{ID: "i2", TenantID: "ten-2", AccountLogin: "other", AccountType: "organization", GitHubInstallationID: 10, Active: true},
	}
	rr = getUI(t, h, "/")
	body = rr.Body.String()
	if rr.Code != http.StatusOK || !strings.Contains(body, "acme") || !strings.Contains(body, "other") || strings.Contains(body, "Install GitHub App") {
		t.Fatalf("picker %d %s", rr.Code, body)
	}
	proj := getUI(t, h, "/projects/p1")
	if proj.Code != http.StatusFound || proj.Header().Get("Location") != "/" {
		t.Fatalf("empty tenant project %d %s", proj.Code, proj.Header().Get("Location"))
	}
}

func TestGitHubCallbackSetsSecureCookie(t *testing.T) {
	store := newUIIdentity("ada", "")
	if err := store.SaveOAuthState(context.Background(), "st", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	gh := &uiGitHub{userID: 4, login: "ada"}
	proj, runsSvc, st := uiServices(&memProjects{items: map[project.ID]*project.Project{}}, &memRuns{items: map[run.ID]*run.Run{}}, &memStates{items: map[string]*state.State{}})
	h := NewServer(ServerConfig{
		Projects: proj, Runs: runsSvc, States: st,
		PublicBaseURL: "https://infra.example",
		Auth: &authapp.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "user-1" },
			PublicBaseURL: "https://infra.example", ClientID: "cid",
		},
		Installations: &installation.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "inst-1" },
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/github/callback?state=st&code=code", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("callback %d %s", rr.Code, rr.Body.Bytes())
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "session" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie %#v", cookie)
	}
}

func TestNewProjectPickerAndCreate(t *testing.T) {
	p := listProject(t)
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	states := &memStates{items: map[string]*state.State{}}

	proj, runsSvc, st := uiServices(projects, runs, states)
	store := newUIIdentity("operator", "ten")
	gh := &uiGitHub{
		slug: "infra-guy", userID: 4, login: "operator",
		membershipState: "active", membershipRole: "admin", membershipStatus: http.StatusOK,
		repos: map[int64][]installation.Repo{
			100: {
				{ID: 101, Owner: "operator", Name: "my-infra"},
				{ID: 102, Owner: "operator", Name: "my-app"},
			},
		},
	}
	h := NewServer(ServerConfig{
		Projects: proj, Runs: runsSvc, States: st,
		Checkout: &stubTip{sha: "abc1234"}, Lookup: &stubLookup{},
		Tokens:        uiTokenSource{store: store},
		PublicBaseURL: "http://localhost:8080",
		Auth: &authapp.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "id-1" },
			PublicBaseURL: "http://localhost:8080", ClientID: "cid",
		},
		Installations: &installation.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "inst-1" },
		},
	})

	// 1. GET /projects/new with active installation renders repo select options
	rr := getUI(t, h, "/projects/new")
	if rr.Code != http.StatusOK {
		t.Fatalf("get /projects/new code=%d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `<select name="repo_id" required>`) || !strings.Contains(body, `<option value="101">operator/my-infra</option>`) {
		t.Fatalf("expected repo options in /projects/new, got %s", body)
	}

	// 2. GET /projects/new when not installed shows Install GitHub App link without form
	noInstStore := newUIIdentity("operator", "ten-no-inst")
	noInstStore.installations = nil
	hNoInst := NewServer(ServerConfig{
		Projects: proj, Runs: runsSvc, States: st,
		Checkout: &stubTip{sha: "abc1234"}, Lookup: &stubLookup{},
		Tokens:        uiTokenSource{store: noInstStore},
		PublicBaseURL: "http://localhost:8080",
		Auth: &authapp.Service{
			Store: noInstStore, GitHub: gh, Now: time.Now, NewID: func() string { return "id-1" },
			PublicBaseURL: "http://localhost:8080", ClientID: "cid",
		},
		Installations: &installation.Service{
			Store: noInstStore, GitHub: gh, Now: time.Now, NewID: func() string { return "inst-1" },
		},
	})
	rrNoInst := getUI(t, hNoInst, "/projects/new")
	if rrNoInst.Code != http.StatusOK {
		t.Fatalf("get /projects/new without install code=%d: %s", rrNoInst.Code, rrNoInst.Body.String())
	}
	bodyNoInst := rrNoInst.Body.String()
	if !strings.Contains(bodyNoInst, "Install GitHub App") || strings.Contains(bodyNoInst, "<form") {
		t.Fatalf("expected Install GitHub App link without form, got %s", bodyNoInst)
	}

	// 3. POST /projects with invalid repo_id returns 400
	rrBad := postUI(t, h, "/projects", strings.NewReader("name=test&repo_id=999&branch=main"))
	if rrBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown repo_id, got %d", rrBad.Code)
	}

	// 4. POST /projects with valid repo_id creates project
	rrCreate := postUI(t, h, "/projects", strings.NewReader("name=test&repo_id=101&branch=main&workdir=infra&auto_approve=on"))
	if rrCreate.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d %s", rrCreate.Code, rrCreate.Body.String())
	}
	created := projects.items["new-p"]
	if created == nil {
		t.Fatal("expected project new-p to be created")
	}
	snap := created.Snapshot()
	if snap.GitHubRepoID != 101 || snap.Repo.Owner != "operator" || snap.Repo.Name != "my-infra" || snap.TenantID != "ten" || !snap.AutoApprove {
		t.Fatalf("unexpected project created: %+v", snap)
	}
}

func uiWithAuth(t *testing.T, projects *memProjects, store *uiIdentity, gh *uiGitHub) http.Handler {
	t.Helper()
	proj, runsSvc, st := uiServices(projects, &memRuns{items: map[run.ID]*run.Run{}}, &memStates{items: map[string]*state.State{}})
	return NewServer(ServerConfig{
		Projects: proj, Runs: runsSvc, States: st,
		Checkout: &stubTip{sha: "abc1234"}, Lookup: &stubLookup{},
		Tokens:        uiTokenSource{store: store},
		PublicBaseURL: "http://localhost:8080",
		Auth: &authapp.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "id-1" },
			PublicBaseURL: "http://localhost:8080", ClientID: "cid",
		},
		Installations: &installation.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "inst-1" },
		},
	})
}

type uiIdentity struct {
	users         []user.User
	tenants       []tenant.Tenant
	installations []tenant.Installation
	sessions      map[string]authapp.Session
	states        map[string]time.Time
}

func newUIIdentity(login, tenantID string) *uiIdentity {
	var installs []tenant.Installation
	if tenantID != "" {
		installs = append(installs, tenant.Installation{
			ID: "inst-" + tenantID, TenantID: tenantID, AccountLogin: login, AccountType: "user", GitHubInstallationID: 100, Active: true,
		})
	}
	return &uiIdentity{
		sessions: map[string]authapp.Session{
			uiSessionID: {
				ID: uiSessionID, UserID: "user-1", TenantID: tenantID, Role: "admin",
				Login: login, AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour),
			},
		},
		installations: installs,
	}
}

func (m *uiIdentity) UpsertUser(_ context.Context, u user.User, _ time.Time) (user.User, error) {
	for i, existing := range m.users {
		if existing.GitHubUserID == u.GitHubUserID {
			existing.Login = u.Login
			existing.DisplayName = u.DisplayName
			m.users[i] = existing
			return existing, nil
		}
	}
	m.users = append(m.users, u)
	return u, nil
}

func (m *uiIdentity) UpsertTenant(_ context.Context, ten tenant.Tenant, _ time.Time) (tenant.Tenant, error) {
	for i, existing := range m.tenants {
		if existing.GitHubAccountID == ten.GitHubAccountID {
			existing.Name = ten.Name
			existing.AccountType = ten.AccountType
			m.tenants[i] = existing
			return existing, nil
		}
	}
	m.tenants = append(m.tenants, ten)
	return ten, nil
}

func (m *uiIdentity) UpsertInstallation(_ context.Context, in tenant.Installation, _ time.Time) error {
	in.Active = true
	m.installations = append(m.installations, in)
	return nil
}

func (m *uiIdentity) ActiveInstallation(_ context.Context, tenantID string) (*tenant.Installation, error) {
	for i := range m.installations {
		if m.installations[i].TenantID == tenantID && m.installations[i].Active {
			in := m.installations[i]
			return &in, nil
		}
	}
	return nil, nil
}

func (m *uiIdentity) ListActiveInstallations(context.Context) ([]tenant.Installation, error) {
	var out []tenant.Installation
	for _, in := range m.installations {
		if in.Active {
			out = append(out, in)
		}
	}
	return out, nil
}

func (m *uiIdentity) SaveSession(_ context.Context, sess authapp.Session) error {
	if m.sessions == nil {
		m.sessions = map[string]authapp.Session{}
	}
	m.sessions[sess.ID] = sess
	return nil
}

func (m *uiIdentity) GetSession(_ context.Context, id string) (*authapp.Session, error) {
	sess, ok := m.sessions[id]
	if !ok {
		return nil, nil
	}
	return &sess, nil
}

func (m *uiIdentity) SaveOAuthState(_ context.Context, state string, expiresAt time.Time) error {
	if m.states == nil {
		m.states = map[string]time.Time{}
	}
	m.states[state] = expiresAt
	return nil
}

func (m *uiIdentity) ConsumeOAuthState(_ context.Context, state string, now time.Time) (bool, error) {
	exp, ok := m.states[state]
	if !ok {
		return false, nil
	}
	delete(m.states, state)
	return !exp.Before(now), nil
}

func (m *uiIdentity) InstallationByGitHubID(_ context.Context, githubInstallationID int64) (*tenant.Installation, error) {
	for i := range m.installations {
		if m.installations[i].GitHubInstallationID == githubInstallationID {
			in := m.installations[i]
			return &in, nil
		}
	}
	return nil, nil
}

func (m *uiIdentity) DeactivateInstallation(_ context.Context, githubInstallationID int64, _ time.Time) error {
	for i := range m.installations {
		if m.installations[i].GitHubInstallationID == githubInstallationID {
			m.installations[i].Active = false
		}
	}
	return nil
}

type uiGitHub struct {
	userID           int64
	login            string
	slug             string
	membershipState  string
	membershipRole   string
	membershipStatus int
	accountID        int64
	accountLogin     string
	accountType      string
	repos            map[int64][]installation.Repo
}

func (f *uiGitHub) ListRepos(ctx context.Context, installationID int64) ([]installation.Repo, error) {
	if f.repos == nil {
		return nil, nil
	}
	return f.repos[installationID], nil
}

func (f *uiGitHub) Exchange(context.Context, string) (string, error) { return "token", nil }

func (f *uiGitHub) CurrentUser(context.Context, string) (int64, string, string, error) {
	return f.userID, f.login, "", nil
}

func (f *uiGitHub) Membership(context.Context, string, string) (string, string, int, error) {
	return f.membershipState, f.membershipRole, f.membershipStatus, nil
}

func (f *uiGitHub) Installation(context.Context, int64) (int64, string, string, error) {
	return f.accountID, f.accountLogin, f.accountType, nil
}

func (f *uiGitHub) Slug(context.Context) (string, error) {
	if f.slug == "" {
		return "infra-guy", nil
	}
	return f.slug, nil
}

type stubVersions struct {
	list []string
}

func (s stubVersions) List(context.Context) []string {
	return s.list
}

type uiMemContexts struct {
	items    map[tfcontext.ID]*tfcontext.Context
	attached map[project.ID][]tfcontext.ID
}

func newUIMemContexts() *uiMemContexts {
	return &uiMemContexts{
		items:    map[tfcontext.ID]*tfcontext.Context{},
		attached: map[project.ID][]tfcontext.ID{},
	}
}

func (m *uiMemContexts) Create(_ context.Context, c *tfcontext.Context) error {
	m.items[c.Snapshot().ID] = tfcontext.Restore(c.Snapshot())
	return nil
}

func (m *uiMemContexts) Get(_ context.Context, id tfcontext.ID, decryptSecrets bool) (*tfcontext.Context, error) {
	c, ok := m.items[id]
	if !ok {
		return nil, nil
	}
	s := c.Snapshot()
	if !decryptSecrets {
		for i := range s.Entries {
			if s.Entries[i].Secret {
				s.Entries[i].Value = ""
			}
		}
	}
	return tfcontext.Restore(s), nil
}

func (m *uiMemContexts) ListByTenant(_ context.Context, tenantID string) ([]*tfcontext.Context, error) {
	var out []*tfcontext.Context
	for _, c := range m.items {
		if c.Snapshot().TenantID == tenantID {
			cp, _ := m.Get(context.Background(), c.Snapshot().ID, false)
			out = append(out, cp)
		}
	}
	return out, nil
}

func (m *uiMemContexts) Delete(_ context.Context, id tfcontext.ID) error {
	delete(m.items, id)
	return nil
}

func (m *uiMemContexts) UpsertEntry(_ context.Context, contextID tfcontext.ID, entry tfcontext.Entry) error {
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

func (m *uiMemContexts) DeleteEntry(_ context.Context, contextID tfcontext.ID, entryID string) error {
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

func (m *uiMemContexts) SetProjectContexts(_ context.Context, projectID project.ID, contextIDs []tfcontext.ID) error {
	m.attached[projectID] = append([]tfcontext.ID(nil), contextIDs...)
	return nil
}

func (m *uiMemContexts) ListByProject(_ context.Context, projectID project.ID, decryptSecrets bool) ([]*tfcontext.Context, error) {
	var out []*tfcontext.Context
	for _, id := range m.attached[projectID] {
		c, _ := m.Get(context.Background(), id, decryptSecrets)
		if c != nil {
			out = append(out, c)
		}
	}
	return out, nil
}

func TestProjectTerraformVersionAndContextsUI(t *testing.T) {
	p := listProject(t)
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	states := &memStates{items: map[string]*state.State{}}
	proj, runsSvc, st := uiServices(projects, runs, states)
	ctxRepo := newUIMemContexts()
	seq := 0
	ctxSvc := &tfcontextapp.Service{
		Contexts: ctxRepo,
		Now:      time.Now,
		NewID: func() string {
			seq++
			return fmt.Sprintf("ctx-%d", seq)
		},
	}
	store := newUIIdentity("operator", "ten")
	gh := &uiGitHub{
		slug: "infra-guy", userID: 4, login: "operator",
		membershipState: "active", membershipRole: "admin", membershipStatus: http.StatusOK,
		repos: map[int64][]installation.Repo{100: {{ID: 101, Owner: "operator", Name: "my-infra"}}},
	}
	h := NewServer(ServerConfig{
		Projects: proj, Runs: runsSvc, States: st, Contexts: ctxSvc,
		Versions:      stubVersions{list: []string{"1.10.5", "1.9.8"}},
		Checkout:      &stubTip{sha: "abc1234"},
		Lookup:        &stubLookup{},
		Tokens:        uiTokenSource{store: store},
		PublicBaseURL: "http://localhost:8080",
		Auth: &authapp.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "id-1" },
			PublicBaseURL: "http://localhost:8080", ClientID: "cid",
		},
		Installations: &installation.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "inst-1" },
		},
	})

	// 1. Create a context
	rr := postUI(t, h, "/contexts", strings.NewReader("name=prod-secrets"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create context %d: %s", rr.Code, rr.Body.String())
	}

	// 1b. GET /contexts lists the created context
	rr = getUI(t, h, "/contexts")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "prod-secrets") {
		t.Fatalf("contexts list %d: %s", rr.Code, rr.Body.String())
	}

	// 2. Add plain_env, secret_env, and secret_file entries
	rr = postUI(t, h, "/contexts/ctx-1/entries", strings.NewReader("kind=plain_env&key=TF_VAR_env&value=prod"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("add plain_env %d: %s", rr.Code, rr.Body.String())
	}
	rr = postUI(t, h, "/contexts/ctx-1/entries", strings.NewReader("kind=secret_env&key=AWS_SECRET_ACCESS_KEY&value=super-secret-value"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("add secret_env %d: %s", rr.Code, rr.Body.String())
	}
	rr = postUI(t, h, "/contexts/ctx-1/entries", strings.NewReader("kind=secret_file&key=.kube/config&value=kube-secret-content"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("add secret_file %d: %s", rr.Code, rr.Body.String())
	}

	// 3. GET /contexts/ctx-1 shows plain value, masks secrets
	rr = getUI(t, h, "/contexts/ctx-1")
	body := rr.Body.String()
	if rr.Code != http.StatusOK || !strings.Contains(body, "TF_VAR_env") || !strings.Contains(body, "prod") {
		t.Fatalf("context page missing plain entry: %s", body)
	}
	if strings.Contains(body, "super-secret-value") || strings.Contains(body, "kube-secret-content") {
		t.Fatalf("context page leaked secret plaintext: %s", body)
	}

	// 4. Create project with terraform_version=1.10.5 and context_ids=ctx-1
	rr = postUI(t, h, "/projects", strings.NewReader("name=test&repo_id=101&branch=main&workdir=infra&terraform_version=1.10.5&context_ids=ctx-1"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create project %d: %s", rr.Code, rr.Body.String())
	}
	created := projects.items["new-p"]
	if created.Snapshot().TerraformVersion != "1.10.5" {
		t.Fatalf("created version = %q, want 1.10.5", created.Snapshot().TerraformVersion)
	}
	attached, _ := ctxSvc.ListByProject(context.Background(), "new-p")
	if len(attached) != 1 || attached[0].Snapshot().ID != "ctx-1" {
		t.Fatalf("attached contexts = %#v", attached)
	}

	// 5. Update project details (terraform_version and context_ids)
	rr = postUI(t, h, "/projects/p1", strings.NewReader("branch=main&workdir=infra&terraform_version=1.10.5&context_ids=ctx-1"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("update project %d: %s", rr.Code, rr.Body.String())
	}
	if projects.items["p1"].Snapshot().TerraformVersion != "1.10.5" {
		t.Fatalf("updated p1 version = %q", projects.items["p1"].Snapshot().TerraformVersion)
	}

	// 6. Verify /projects/p1/details and /projects/p1 render version and contexts
	rr = getUI(t, h, "/projects/p1/details")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "1.10.5") || !strings.Contains(rr.Body.String(), "prod-secrets") {
		t.Fatalf("project details %d: %s", rr.Code, rr.Body.String())
	}
	rr = getUI(t, h, "/projects/p1")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "1.10.5") {
		t.Fatalf("project page meta version %d: %s", rr.Code, rr.Body.String())
	}

	// 7. Delete entry and delete context
	rr = postUI(t, h, "/contexts/ctx-1/entries/ctx-2/delete", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete entry %d: %s", rr.Code, rr.Body.String())
	}
	rr = postUI(t, h, "/contexts/ctx-1/delete", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete context %d: %s", rr.Code, rr.Body.String())
	}
	if got, _ := ctxSvc.Get(context.Background(), "ten", "ctx-1"); got != nil {
		t.Fatalf("expected ctx-1 deleted, got %v", got)
	}
}

type uiMemPolicies struct {
	items    map[policy.ID]*policy.Policy
	projects *memProjects
}

func newUIMemPolicies(projects *memProjects) *uiMemPolicies {
	return &uiMemPolicies{
		items:    map[policy.ID]*policy.Policy{},
		projects: projects,
	}
}

func (m *uiMemPolicies) Insert(_ context.Context, p *policy.Policy) error {
	for _, existing := range m.items {
		if existing.TenantID() == p.TenantID() && existing.Name() == p.Name() {
			return policy.ErrDuplicateName
		}
	}
	m.items[p.ID()] = policy.Restore(p.Snapshot())
	return nil
}

func (m *uiMemPolicies) Update(_ context.Context, p *policy.Policy) error {
	if _, ok := m.items[p.ID()]; !ok {
		return policy.ErrNotFound
	}
	for id, existing := range m.items {
		if id != p.ID() && existing.TenantID() == p.TenantID() && existing.Name() == p.Name() {
			return policy.ErrDuplicateName
		}
	}
	m.items[p.ID()] = policy.Restore(p.Snapshot())
	return nil
}

func (m *uiMemPolicies) Delete(_ context.Context, id policy.ID) error {
	if _, ok := m.items[id]; !ok {
		return policy.ErrNotFound
	}
	if m.projects != nil {
		for _, proj := range m.projects.items {
			if proj.Snapshot().ApprovalPolicyID == string(id) {
				return errors.New("policy is attached to project")
			}
		}
	}
	delete(m.items, id)
	return nil
}

func (m *uiMemPolicies) Get(_ context.Context, id policy.ID) (*policy.Policy, error) {
	p, ok := m.items[id]
	if !ok {
		return nil, nil
	}
	return policy.Restore(p.Snapshot()), nil
}

func (m *uiMemPolicies) ListByTenant(_ context.Context, tenantID string) ([]*policy.Policy, error) {
	var out []*policy.Policy
	for _, p := range m.items {
		if p.TenantID() == tenantID {
			out = append(out, policy.Restore(p.Snapshot()))
		}
	}
	return out, nil
}

func TestPoliciesUIAndApprovalEnforcement(t *testing.T) {
	p := listProject(t)
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	runs := &memRuns{items: map[run.ID]*run.Run{}}
	states := &memStates{items: map[string]*state.State{}}
	projSvc, runsSvc, stSvc := uiServices(projects, runs, states)

	polRepo := newUIMemPolicies(projects)
	polSeq := 0
	polSvc := &policyapp.Service{
		Policies: polRepo,
		Engine:   opa.NewEvaluator(),
		Now:      time.Now,
		NewID: func() string {
			polSeq++
			return fmt.Sprintf("pol-%d", polSeq)
		},
	}

	store := newUIIdentity("operator", "ten")
	gh := &uiGitHub{
		slug: "infra-guy", userID: 4, login: "operator",
		membershipState: "active", membershipRole: "admin", membershipStatus: http.StatusOK,
		repos: map[int64][]installation.Repo{100: {{ID: 101, Owner: "operator", Name: "my-infra"}}},
	}
	h := NewServer(ServerConfig{
		Projects:      projSvc,
		Runs:          runsSvc,
		States:        stSvc,
		Policies:      polSvc,
		Versions:      stubVersions{list: []string{"1.10.5"}},
		Checkout:      &stubTip{sha: "abc1234"},
		Lookup:        &stubLookup{},
		Tokens:        uiTokenSource{store: store},
		PublicBaseURL: "http://localhost:8080",
		Auth: &authapp.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "id-1" },
			PublicBaseURL: "http://localhost:8080", ClientID: "cid",
		},
		Installations: &installation.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "inst-1" },
		},
	})

	// 1. GET /policies renders the list and Online Policy Editor & Tester
	rr := getUI(t, h, "/policies")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /policies = %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/policies"`) || !strings.Contains(body, `id="policy-rego"`) || !strings.Contains(body, `data-code-editor="rego"`) || !strings.Contains(body, `id="policy-input-json"`) || !strings.Contains(body, `data-code-editor="json"`) || !strings.Contains(body, `/static/editor.js`) || !strings.Contains(body, "Test Policy") || !strings.Contains(body, "package infraguy.approval") {
		t.Fatalf("GET /policies missing editor/tester elements: %s", body)
	}
	rrStatic := getUI(t, h, "/static/editor.js")
	if rrStatic.Code != http.StatusOK || !strings.Contains(rrStatic.Body.String(), "highlightRego") {
		t.Fatalf("GET /static/editor.js = %d: %s", rrStatic.Code, rrStatic.Body.String())
	}

	// 2. POST /policies/test evaluates Rego against input JSON (allowed, denied, and invalid Rego)
	denyRego := `package infraguy.approval
import rego.v1
default allow := false
allow if { count(deny) == 0 }
deny contains "Cannot destroy S3 buckets" if {
	some rc in input.plan.resource_changes
	rc.type == "aws_s3_bucket"
	"delete" in rc.change.actions
}
`
	testPayloadAllowed, _ := json.Marshal(map[string]any{
		"rego":       denyRego,
		"input_json": `{"plan":{"resource_changes":[{"type":"aws_s3_bucket","change":{"actions":["create"]}}]}}`,
	})
	reqAllow := httptest.NewRequest(http.MethodPost, "/policies/test", strings.NewReader(string(testPayloadAllowed)))
	reqAllow.Header.Set("Content-Type", "application/json")
	reqAllow.AddCookie(&http.Cookie{Name: "session", Value: uiSessionID})
	rrAllow := httptest.NewRecorder()
	h.ServeHTTP(rrAllow, reqAllow)
	if rrAllow.Code != http.StatusOK {
		t.Fatalf("POST /policies/test (allowed) = %d: %s", rrAllow.Code, rrAllow.Body.String())
	}
	var allowResp struct {
		Allowed bool     `json:"allowed"`
		Deny    []string `json:"deny"`
	}
	if err := json.Unmarshal(rrAllow.Body.Bytes(), &allowResp); err != nil || !allowResp.Allowed || len(allowResp.Deny) != 0 {
		t.Fatalf("unexpected allow response: %s (err=%v)", rrAllow.Body.String(), err)
	}

	testPayloadDenied, _ := json.Marshal(map[string]any{
		"rego":       denyRego,
		"input_json": `{"plan":{"resource_changes":[{"type":"aws_s3_bucket","change":{"actions":["delete"]}}]}}`,
	})
	reqDeny := httptest.NewRequest(http.MethodPost, "/policies/test", strings.NewReader(string(testPayloadDenied)))
	reqDeny.Header.Set("Content-Type", "application/json")
	reqDeny.AddCookie(&http.Cookie{Name: "session", Value: uiSessionID})
	rrDeny := httptest.NewRecorder()
	h.ServeHTTP(rrDeny, reqDeny)
	if rrDeny.Code != http.StatusOK {
		t.Fatalf("POST /policies/test (denied) = %d: %s", rrDeny.Code, rrDeny.Body.String())
	}
	var denyResp struct {
		Allowed bool     `json:"allowed"`
		Deny    []string `json:"deny"`
	}
	if err := json.Unmarshal(rrDeny.Body.Bytes(), &denyResp); err != nil || denyResp.Allowed || len(denyResp.Deny) != 1 || denyResp.Deny[0] != "Cannot destroy S3 buckets" {
		t.Fatalf("unexpected deny response: %s (err=%v)", rrDeny.Body.String(), err)
	}

	// Invalid Rego returns 400 with JSON error
	rrBadRego := postUI(t, h, "/policies/test", strings.NewReader(url.Values{
		"rego":       {"package infraguy.approval\nthis is not valid rego {{{"},
		"input_json": {"{}"},
	}.Encode()))
	if rrBadRego.Code != http.StatusBadRequest || !strings.Contains(rrBadRego.Body.String(), `"error"`) {
		t.Fatalf("POST /policies/test invalid rego = %d: %s", rrBadRego.Code, rrBadRego.Body.String())
	}

	// 3. POST /policies creates a new policy (and rejects invalid Rego)
	rrInvalidCreate := postUI(t, h, "/policies", strings.NewReader(url.Values{
		"name": {"broken"},
		"rego": {"not valid rego"},
	}.Encode()))
	if rrInvalidCreate.Code != http.StatusBadRequest {
		t.Fatalf("POST /policies invalid rego = %d, want 400", rrInvalidCreate.Code)
	}

	rrCreate := postUI(t, h, "/policies", strings.NewReader(url.Values{
		"name": {"no-s3-delete"},
		"rego": {denyRego},
	}.Encode()))
	if rrCreate.Code != http.StatusSeeOther || rrCreate.Header().Get("Location") != "/policies/pol-2" {
		t.Fatalf("POST /policies = %d (loc=%q): %s", rrCreate.Code, rrCreate.Header().Get("Location"), rrCreate.Body.String())
	}

	// 4. GET /policies/pol-2 renders policy editor and tester
	rrPol := getUI(t, h, "/policies/pol-2")
	if rrPol.Code != http.StatusOK || !strings.Contains(rrPol.Body.String(), "no-s3-delete") || !strings.Contains(rrPol.Body.String(), "Cannot destroy S3 buckets") {
		t.Fatalf("GET /policies/pol-2 = %d: %s", rrPol.Code, rrPol.Body.String())
	}

	// 5. Assign policy to project via POST /projects/p1/policy (and verify settings page vs minimal project page)
	rrProjPage := getUI(t, h, "/projects/p1")
	if rrProjPage.Code != http.StatusOK || strings.Contains(rrProjPage.Body.String(), `name="approval_policy_id"`) {
		t.Fatalf("expected /projects/p1 to be minimal without policy form, got %d: %s", rrProjPage.Code, rrProjPage.Body.String())
	}
	rrSettings := getUI(t, h, "/projects/p1/settings")
	if rrSettings.Code != http.StatusOK || !strings.Contains(rrSettings.Body.String(), `name="approval_policy_id"`) {
		t.Fatalf("expected /projects/p1/settings to render approval_policy_id select, got %d: %s", rrSettings.Code, rrSettings.Body.String())
	}
	rrAssign := postUI(t, h, "/projects/p1/policy", strings.NewReader("approval_policy_id=pol-2"))
	if rrAssign.Code != http.StatusSeeOther {
		t.Fatalf("POST /projects/p1/policy = %d: %s", rrAssign.Code, rrAssign.Body.String())
	}
	if projects.items["p1"].Snapshot().ApprovalPolicyID != "pol-2" {
		t.Fatalf("expected p1 ApprovalPolicyID=pol-2, got %q", projects.items["p1"].Snapshot().ApprovalPolicyID)
	}

	// 6. Deleting a policy attached to a project fails with 409 Conflict
	rrDelAttached := postUI(t, h, "/policies/pol-2/delete", nil)
	if rrDelAttached.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict deleting attached policy, got %d: %s", rrDelAttached.Code, rrDelAttached.Body.String())
	}

	// 7. Run with destructive S3 change is denied in UI and blocked on POST confirm (403 Forbidden)
	denySnap := needsApprovalSnap("r-deny")
	denySnap.PlanJSON = []byte(`{"resource_changes":[{"address":"aws_s3_bucket.main","type":"aws_s3_bucket","change":{"actions":["delete"]}}]}`)
	runs.items["r-deny"] = run.Restore(denySnap)

	rrRunDeny := getUI(t, h, "/runs/r-deny")
	runDenyBody := rrRunDeny.Body.String()
	if rrRunDeny.Code != http.StatusOK {
		t.Fatalf("GET /runs/r-deny = %d: %s", rrRunDeny.Code, runDenyBody)
	}
	if !strings.Contains(runDenyBody, "Cannot destroy S3 buckets") || !strings.Contains(runDenyBody, "Denied") || !strings.Contains(runDenyBody, "Approval blocked by policy") {
		t.Fatalf("expected Denied status, deny message, and disabled button on /runs/r-deny, got: %s", runDenyBody)
	}
	if strings.Contains(runDenyBody, "Policy Input JSON (for Policy Tester)") {
		t.Fatalf("did not expect Policy Input JSON collapsible section on /runs/r-deny, got: %s", runDenyBody)
	}

	// Confirm via /runs/r-deny/confirm -> 403 Forbidden
	rrConfirmDeny := postUI(t, h, "/runs/r-deny/confirm", nil)
	if rrConfirmDeny.Code != http.StatusForbidden || !strings.Contains(rrConfirmDeny.Body.String(), "Cannot destroy S3 buckets") {
		t.Fatalf("expected 403 Forbidden on /runs/r-deny/confirm, got %d: %s", rrConfirmDeny.Code, rrConfirmDeny.Body.String())
	}
	if runs.items["r-deny"].Snapshot().Status != run.StatusNeedsApproval {
		t.Fatalf("expected r-deny to remain needs_approval, got %s", runs.items["r-deny"].Snapshot().Status)
	}

	// Confirm via /projects/p1/runs/r-deny/confirm -> 403 Forbidden
	rrConfirmDenyProj := postUI(t, h, "/projects/p1/runs/r-deny/confirm", nil)
	if rrConfirmDenyProj.Code != http.StatusForbidden || !strings.Contains(rrConfirmDenyProj.Body.String(), "Cannot destroy S3 buckets") {
		t.Fatalf("expected 403 Forbidden on /projects/p1/runs/r-deny/confirm, got %d: %s", rrConfirmDenyProj.Code, rrConfirmDenyProj.Body.String())
	}

	// 8. Run with safe S3 create is allowed in UI and confirmed on POST confirm (303 See Other)
	allowSnap := needsApprovalSnap("r-allow")
	allowSnap.PlanJSON = []byte(`{"resource_changes":[{"address":"aws_s3_bucket.logs","type":"aws_s3_bucket","change":{"actions":["create"]}}]}`)
	runs.items["r-allow"] = run.Restore(allowSnap)

	rrRunAllow := getUI(t, h, "/projects/p1/runs/r-allow")
	runAllowBody := rrRunAllow.Body.String()
	if rrRunAllow.Code != http.StatusOK || !strings.Contains(runAllowBody, "Allowed") || strings.Contains(runAllowBody, "Approval blocked by policy") {
		t.Fatalf("expected Allowed status on /projects/p1/runs/r-allow, got %d: %s", rrRunAllow.Code, runAllowBody)
	}

	rrConfirmAllow := postUI(t, h, "/projects/p1/runs/r-allow/confirm", nil)
	if rrConfirmAllow.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 See Other on /projects/p1/runs/r-allow/confirm, got %d: %s", rrConfirmAllow.Code, rrConfirmAllow.Body.String())
	}
	if runs.items["r-allow"].Snapshot().ApprovedBy != "operator" {
		t.Fatalf("expected r-allow ApprovedBy=operator, got %q", runs.items["r-allow"].Snapshot().ApprovedBy)
	}

	// 9. Multi-person approval policy (requires > 2 approvals, undecided until > 2 approvals)
	quorumRego := `package infraguy.approval
import rego.v1
allow if {
	count(input.approvals) > 2
}
`
	rrCreateQuorum := postUI(t, h, "/policies", strings.NewReader(url.Values{
		"name": {"quorum-3"},
		"rego": {quorumRego},
	}.Encode()))
	if rrCreateQuorum.Code != http.StatusSeeOther || rrCreateQuorum.Header().Get("Location") != "/policies/pol-3" {
		t.Fatalf("POST /policies (quorum-3) = %d: %s", rrCreateQuorum.Code, rrCreateQuorum.Body.String())
	}
	rrAssignQuorum := postUI(t, h, "/projects/p1/policy", strings.NewReader("approval_policy_id=pol-3"))
	if rrAssignQuorum.Code != http.StatusSeeOther {
		t.Fatalf("assign quorum-3 = %d", rrAssignQuorum.Code)
	}

	quorumSnap := needsApprovalSnap("r-quorum")
	quorumSnap.PlanJSON = []byte(`{"format_version":"1.2"}`)
	runs.items["r-quorum"] = run.Restore(quorumSnap)

	// 0 approvals -> Undecided in UI and 403 Forbidden on Confirm
	rrQuorum0 := getUI(t, h, "/runs/r-quorum")
	if rrQuorum0.Code != http.StatusOK || !strings.Contains(rrQuorum0.Body.String(), "Undecided") || !strings.Contains(rrQuorum0.Body.String(), "Approval undecided by policy") {
		t.Fatalf("expected Undecided on /runs/r-quorum with 0 approvals, got %d: %s", rrQuorum0.Code, rrQuorum0.Body.String())
	}
	if rr0 := postUI(t, h, "/runs/r-quorum/confirm", nil); rr0.Code != http.StatusForbidden || !strings.Contains(rr0.Body.String(), "Approval undecided by policy") {
		t.Fatalf("expected 403 Forbidden (undecided) with 0 approvals, got %d: %s", rr0.Code, rr0.Body.String())
	}

	// 1st approval (operator via HTTP POST /runs/r-quorum/approve)
	if rrApp1 := postUI(t, h, "/runs/r-quorum/approve", nil); rrApp1.Code != http.StatusSeeOther {
		t.Fatalf("POST /runs/r-quorum/approve = %d: %s", rrApp1.Code, rrApp1.Body.String())
	}
	// Duplicate approval by same user is idempotent
	if rrAppDup := postUI(t, h, "/runs/r-quorum/approve", nil); rrAppDup.Code != http.StatusSeeOther {
		t.Fatalf("duplicate approve = %d", rrAppDup.Code)
	}
	if len(runs.items["r-quorum"].Snapshot().Approvals) != 1 {
		t.Fatalf("expected 1 approval after duplicate, got %d", len(runs.items["r-quorum"].Snapshot().Approvals))
	}
	if rr1 := postUI(t, h, "/runs/r-quorum/confirm", nil); rr1.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden with 1 approval, got %d", rr1.Code)
	}

	// 2nd approval (bob) -> still <= 2 approvals, so Confirm is still blocked
	if err := runsSvc.Approve(context.Background(), "r-quorum", run.Approval{UserID: "u-bob", Login: "bob", Role: "member"}); err != nil {
		t.Fatalf("approve bob: %v", err)
	}
	if rr2 := postUI(t, h, "/runs/r-quorum/confirm", nil); rr2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden with 2 approvals, got %d", rr2.Code)
	}

	// 3rd approval (charlie) -> > 2 approvals, now Allowed and Confirm succeeds!
	if err := runsSvc.Approve(context.Background(), "r-quorum", run.Approval{UserID: "u-charlie", Login: "charlie", Role: "member"}); err != nil {
		t.Fatalf("approve charlie: %v", err)
	}
	rrQuorumPage := getUI(t, h, "/runs/r-quorum")
	if rrQuorumPage.Code != http.StatusOK || !strings.Contains(rrQuorumPage.Body.String(), "Allowed") || !strings.Contains(rrQuorumPage.Body.String(), "Approved by operator, bob, charlie (3)") {
		t.Fatalf("expected Allowed and 3 approvals on /runs/r-quorum, got %d: %s", rrQuorumPage.Code, rrQuorumPage.Body.String())
	}
	if rr3 := postUI(t, h, "/runs/r-quorum/confirm", nil); rr3.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 See Other with 3 approvals, got %d: %s", rr3.Code, rr3.Body.String())
	}

	// 10. Detach policy from project and delete policies
	rrDetach := postUI(t, h, "/projects/p1/policy", strings.NewReader("approval_policy_id="))
	if rrDetach.Code != http.StatusSeeOther || projects.items["p1"].Snapshot().ApprovalPolicyID != "" {
		t.Fatalf("detach policy = %d, ApprovalPolicyID=%q", rrDetach.Code, projects.items["p1"].Snapshot().ApprovalPolicyID)
	}
	rrDel := postUI(t, h, "/policies/pol-2/delete", nil)
	if rrDel.Code != http.StatusSeeOther || rrDel.Header().Get("Location") != "/policies" {
		t.Fatalf("delete policy = %d: %s", rrDel.Code, rrDel.Body.String())
	}
	if _, err := polSvc.Get(context.Background(), "ten", "pol-2"); !errors.Is(err, policy.ErrNotFound) {
		t.Fatalf("expected pol-2 to be deleted, err=%v", err)
	}
}

func TestRunPlanStepStaysOpenWhenUnconfirmedAndRendersParsedPlanJSON(t *testing.T) {
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": testProject(t)}}
	snap := needsApprovalSnap("r-plan-json")
	snap.Steps[1].Logs = "Refreshing state...\n"
	snap.PlanText = "Plan: 1 to add, 1 to change, 0 to destroy."
	snap.PlanJSON = []byte(`{
		"format_version": "1.2",
		"resource_changes": [
			{
				"address": "aws_s3_bucket.assets",
				"type": "aws_s3_bucket",
				"name": "assets",
				"change": {
					"actions": ["create"],
					"before": null,
					"after": {"bucket": "my-assets-bucket"},
					"after_unknown": {"arn": true}
				}
			},
			{
				"address": "aws_iam_role.worker",
				"type": "aws_iam_role",
				"name": "worker",
				"change": {
					"actions": ["update"],
					"before": {"description": "old"},
					"after": {"description": "new"},
					"after_unknown": {}
				}
			}
		],
		"output_changes": {
			"bucket_name": {
				"actions": ["create"],
				"before": null,
				"after": "my-assets-bucket",
				"after_unknown": false
			}
		}
	}`)
	runs := &memRuns{items: map[run.ID]*run.Run{"r-plan-json": run.Restore(snap)}}
	h := uiHandler(t, projects, runs, &memStates{items: map[string]*state.State{}}, "")

	// 1. /runs/r-plan-json/history keeps the Plan step <details> open while awaiting approval and renders raw step logs (not the structured diff)
	histRR := getUI(t, h, "/runs/r-plan-json/history")
	histBody := histRR.Body.String()
	if histRR.Code != http.StatusOK {
		t.Fatalf("history status = %d: %s", histRR.Code, histBody)
	}
	if !strings.Contains(histBody, `<details data-step="plan" data-status="succeeded" open>`) {
		t.Fatalf("expected plan <details> to stay open when awaiting approval, got:\n%s", histBody)
	}
	if !strings.Contains(histBody, "Refreshing state...") {
		t.Fatalf("expected history body to contain raw step logs %q, got:\n%s", "Refreshing state...", histBody)
	}
	if strings.Contains(histBody, `class="plan-diff"`) {
		t.Fatalf("expected history tab NOT to render structured plan-diff cards, got:\n%s", histBody)
	}

	// 2. /runs/r-plan-json/changes renders structured plan diff with resource cards collapsed by default
	chgRR := getUI(t, h, "/runs/r-plan-json/changes")
	chgBody := chgRR.Body.String()
	if chgRR.Code != http.StatusOK {
		t.Fatalf("changes status = %d: %s", chgRR.Code, chgBody)
	}
	for _, want := range []string{
		`<details class="plan-resource plan-resource-create" data-preserve-key="res:aws_s3_bucket.assets">`,
		"aws_s3_bucket.assets",
		"aws_iam_role.worker",
		"my-assets-bucket",
		"(known after apply)",
		"bucket_name",
		"Raw Terraform plan output",
	} {
		if !strings.Contains(chgBody, want) {
			t.Fatalf("expected changes body to contain %q, got:\n%s", want, chgBody)
		}
	}

	// 3. Running step renders live streaming indicator and data-log-step attribute
	runningSnap := run.Snapshot{
		ID: "r-running", ProjectID: "p1", Trigger: run.TriggerPush, Status: run.StatusRunning,
		SHA: "abc1234def",
		Steps: []run.Step{
			{Name: run.StepInit, Position: 1, Status: run.StepSucceeded, Logs: "init done"},
			{Name: run.StepPlan, Position: 2, Status: run.StepRunning, Logs: "aws_s3_bucket.assets: Refreshing state..."},
		},
	}
	runs.items["r-running"] = run.Restore(runningSnap)
	liveRR := getUI(t, h, "/runs/r-running/history")
	liveBody := liveRR.Body.String()
	if liveRR.Code != http.StatusOK {
		t.Fatalf("live history status = %d: %s", liveRR.Code, liveBody)
	}
	for _, want := range []string{
		`<details data-step="plan" data-status="running" open>`,
		`streaming&hellip;`,
		`data-log-step="plan"`,
		`aws_s3_bucket.assets: Refreshing state...`,
	} {
		if !strings.Contains(liveBody, want) {
			t.Fatalf("expected live history body to contain %q, got:\n%s", want, liveBody)
		}
	}
}

func TestPRRunApprovalUI(t *testing.T) {
	p := listProject(t)
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	prNum := 42
	prSnap := run.Snapshot{
		ID: "r-pr-1", ProjectID: "p1", Trigger: run.TriggerPullRequest, Status: run.StatusPlanned,
		PlanOnly: true, PRNumber: &prNum, PRTitle: "Add S3 bucket", SHA: "abc1234def",
		Add: 3, Destroy: 1, SummaryPresent: true,
		PlanJSON: []byte(`{"format_version":"1.2"}`),
		Steps: []run.Step{
			{Name: run.StepInit, Position: 1, Status: run.StepSucceeded, Logs: "init done"},
			{Name: run.StepPlan, Position: 2, Status: run.StepSucceeded, Logs: "plan done"},
		},
	}
	runs := &memRuns{items: map[run.ID]*run.Run{"r-pr-1": run.Restore(prSnap)}}
	states := &memStates{items: map[string]*state.State{}}
	projSvc, runsSvc, stSvc := uiServices(projects, runs, states)
	polRepo := newUIMemPolicies(projects)
	polSeq := 0
	polSvc := &policyapp.Service{
		Policies: polRepo,
		Engine:   opa.NewEvaluator(),
		Now:      time.Now,
		NewID: func() string {
			polSeq++
			return fmt.Sprintf("pol-%d", polSeq)
		},
	}
	store := newUIIdentity("operator", "ten")
	gh := &uiGitHub{
		slug: "infra-guy", userID: 4, login: "operator",
		membershipState: "active", membershipRole: "admin", membershipStatus: http.StatusOK,
		repos: map[int64][]installation.Repo{100: {{ID: 101, Owner: "operator", Name: "my-infra"}}},
	}
	h := NewServer(ServerConfig{
		Projects:      projSvc,
		Runs:          runsSvc,
		States:        stSvc,
		Policies:      polSvc,
		Versions:      stubVersions{list: []string{"1.10.5"}},
		Checkout:      &stubTip{sha: "abc1234"},
		Lookup:        &stubLookup{},
		Tokens:        uiTokenSource{store: store},
		PublicBaseURL: "http://localhost:8080",
		Auth: &authapp.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "id-1" },
			PublicBaseURL: "http://localhost:8080", ClientID: "cid",
		},
		Installations: &installation.Service{
			Store: store, GitHub: gh, Now: time.Now, NewID: func() string { return "inst-1" },
		},
	})

	// Attach a policy requiring >= 1 approval
	rego := `package infraguy.approval
import rego.v1
allow if {
	count(input.approvals) >= 1
}
`
	rrPol := postUI(t, h, "/policies", strings.NewReader(url.Values{"name": {"pr-approval"}, "rego": {rego}}.Encode()))
	if rrPol.Code != http.StatusSeeOther {
		t.Fatalf("create policy = %d: %s", rrPol.Code, rrPol.Body.String())
	}
	if rrAssign := postUI(t, h, "/projects/p1/policy", strings.NewReader("approval_policy_id=pol-1")); rrAssign.Code != http.StatusSeeOther {
		t.Fatalf("assign policy = %d", rrAssign.Code)
	}

	// 1. Before approval: PR run shows Approve and Decline/Cancel buttons, and Undecided policy status, but NOT Confirm button
	rrBefore := getUI(t, h, "/runs/r-pr-1")
	bodyBefore := rrBefore.Body.String()
	if rrBefore.Code != http.StatusOK {
		t.Fatalf("GET /runs/r-pr-1 = %d: %s", rrBefore.Code, bodyBefore)
	}
	if !strings.Contains(bodyBefore, `action="/runs/r-pr-1/approve"`) {
		t.Fatalf("expected Approve form on planned PR run with changes, got:\n%s", bodyBefore)
	}
	if !strings.Contains(bodyBefore, `action="/runs/r-pr-1/cancel"`) {
		t.Fatalf("expected Cancel/Decline form on planned PR run, got:\n%s", bodyBefore)
	}
	if !strings.Contains(bodyBefore, "Undecided") || !strings.Contains(bodyBefore, "Approval undecided by policy") {
		t.Fatalf("expected Undecided policy evaluation on planned PR run, got:\n%s", bodyBefore)
	}
	if strings.Contains(bodyBefore, `/runs/r-pr-1/confirm`) {
		t.Fatalf("expected PlanOnly PR run NOT to render Confirm button, got:\n%s", bodyBefore)
	}

	// 2. Approve PR run via POST /runs/r-pr-1/approve
	rrApprove := postUI(t, h, "/runs/r-pr-1/approve", nil)
	if rrApprove.Code != http.StatusSeeOther {
		t.Fatalf("POST /runs/r-pr-1/approve = %d: %s", rrApprove.Code, rrApprove.Body.String())
	}

	// 3. After approval: policy evaluation is Allowed, Approved by operator (1) is shown, and Unapprove + Decline/Cancel buttons are shown
	rrAfter := getUI(t, h, "/runs/r-pr-1")
	bodyAfter := rrAfter.Body.String()
	if !strings.Contains(bodyAfter, "Allowed") || !strings.Contains(bodyAfter, "Approved by operator (1)") {
		t.Fatalf("expected Allowed and Approved by operator (1) after approval, got:\n%s", bodyAfter)
	}
	if !strings.Contains(bodyAfter, `action="/runs/r-pr-1/unapprove"`) || !strings.Contains(bodyAfter, "Unapprove (1)") {
		t.Fatalf("expected Unapprove (1) form after approval, got:\n%s", bodyAfter)
	}
	if !strings.Contains(bodyAfter, `action="/runs/r-pr-1/cancel"`) {
		t.Fatalf("expected Cancel/Decline form after approval, got:\n%s", bodyAfter)
	}

	// 4. Unapprove PR run via POST /runs/r-pr-1/unapprove
	rrUnapprove := postUI(t, h, "/runs/r-pr-1/unapprove", nil)
	if rrUnapprove.Code != http.StatusSeeOther {
		t.Fatalf("POST /runs/r-pr-1/unapprove = %d: %s", rrUnapprove.Code, rrUnapprove.Body.String())
	}
	rrAfterUnapprove := getUI(t, h, "/runs/r-pr-1")
	bodyAfterUnapprove := rrAfterUnapprove.Body.String()
	if !strings.Contains(bodyAfterUnapprove, `action="/runs/r-pr-1/approve"`) || !strings.Contains(bodyAfterUnapprove, "Undecided") {
		t.Fatalf("expected Approve form and Undecided after unapprove, got:\n%s", bodyAfterUnapprove)
	}

	// 5. Decline/cancel PR run via POST /runs/r-pr-1/cancel
	rrCancel := postUI(t, h, "/runs/r-pr-1/cancel", nil)
	if rrCancel.Code != http.StatusSeeOther {
		t.Fatalf("POST /runs/r-pr-1/cancel = %d: %s", rrCancel.Code, rrCancel.Body.String())
	}
	rrAfterCancel := getUI(t, h, "/runs/r-pr-1")
	bodyAfterCancel := rrAfterCancel.Body.String()
	if !strings.Contains(bodyAfterCancel, "Cancelled by operator") {
		t.Fatalf("expected Cancelled by operator after cancel, got:\n%s", bodyAfterCancel)
	}
}


