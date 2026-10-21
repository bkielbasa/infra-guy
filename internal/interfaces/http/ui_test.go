package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authapp "infra-guy/internal/application/auth"
	"infra-guy/internal/application/installation"
	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	stateapp "infra-guy/internal/application/state"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/state"
	"infra-guy/internal/domain/tenant"
	"infra-guy/internal/domain/user"
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
