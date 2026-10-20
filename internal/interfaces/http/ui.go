package httpapi

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/state"
)

//go:embed templates/*.html static/*
var content embed.FS

var pages = template.Must(template.ParseFS(content, "templates/*.html"))

type ui struct{ cfg ServerConfig }

type layoutData struct {
	Title string
	Body  template.HTML
}

type metaView struct {
	TerraformVersion, Repo, Workdir, Branch, SHA, StateUpdated string
}

type projectRow struct {
	ID, Name, Repo, Branch, Status, StatusClass string
}

type runRow struct {
	ID, Status, StatusClass, Date, TriggeredBy, TerraformVersion string
	Delta                                                        template.HTML
	sortAt                                                       time.Time
}

type runGroup struct {
	Title, Author, Date, SHA string
	Runs                     []runRow
}

type tabView struct {
	ProjectID string
	Poll      bool
	Groups    []runGroup
	Entries   []stateRow
}

type stateRow struct {
	Time, RunID, Serial, Size, JSON string
}

type stepView struct {
	Open bool
	Logs string
}

type historyView struct {
	ID, StatusLabel, StatusClass, ApprovedBy, QueuedAt string
	Poll, Terminal, Unconfirmed, Confirmed, Queued     bool
	Apply, Plan, Init                                  *stepView
}

func (u *ui) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", u.list)
	mux.HandleFunc("GET /projects/new", u.newProject)
	mux.HandleFunc("POST /projects", u.create)
	mux.HandleFunc("GET /projects/{id}", u.project)
	mux.HandleFunc("GET /projects/{id}/headline", u.projectHeadline)
	mux.HandleFunc("GET /projects/{id}/tabs/tracked", u.tabTracked)
	mux.HandleFunc("GET /projects/{id}/tabs/pulls", u.tabPulls)
	mux.HandleFunc("GET /projects/{id}/tabs/state", u.tabState)
	mux.HandleFunc("GET /projects/{id}/details", u.details)
	mux.HandleFunc("POST /projects/{id}", u.update)
	mux.HandleFunc("POST /projects/{id}/lock", u.lock)
	mux.HandleFunc("POST /projects/{id}/trigger", u.trigger)
	mux.HandleFunc("POST /projects/{id}/force-unlock", u.forceUnlock)
	mux.HandleFunc("POST /projects/{id}/delete", u.delete)
	mux.HandleFunc("GET /runs/{id}", u.runPage)
	mux.HandleFunc("GET /runs/{id}/headline", u.runHeadline)
	mux.HandleFunc("GET /runs/{id}/history", u.history)
	mux.HandleFunc("GET /runs/{id}/changes", u.changes)
	mux.HandleFunc("POST /runs/{id}/confirm", u.confirm)
	mux.HandleFunc("POST /runs/{id}/cancel", u.cancel)
	mux.Handle("GET /static/", http.FileServer(http.FS(content)))
}

func (u *ui) actor(r *http.Request) string {
	if u.cfg.BasicAuthUser == "" || u.cfg.BasicAuthPassword == "" {
		return "operator"
	}
	user, _, ok := r.BasicAuth()
	if !ok || user == "" {
		return "operator"
	}
	return user
}

func (u *ui) page(w http.ResponseWriter, title, name string, data any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "layout", layoutData{Title: title, Body: template.HTML(buf.String())}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (u *ui) fragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (u *ui) list(w http.ResponseWriter, r *http.Request) {
	items, err := u.cfg.Projects.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]projectRow, 0, len(items))
	for _, p := range items {
		ps := p.Snapshot()
		row := projectRow{ID: string(ps.ID), Name: ps.Name, Repo: ps.Repo.String(), Branch: ps.DefaultBranch}
		runs, err := u.cfg.Runs.ListByProject(r.Context(), ps.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if latest := latestSnapshot(runs); latest != nil {
			row.Status = latest.Status.Label()
			row.StatusClass = statusClass(latest.Status)
		}
		rows = append(rows, row)
	}
	u.page(w, "Projects", "projects", map[string]any{"Projects": rows})
}

func (u *ui) newProject(w http.ResponseWriter, r *http.Request) {
	u.page(w, "New project", "new_project", nil)
}

func (u *ui) create(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := u.cfg.Projects.Create(r.Context(), r.FormValue("name"), r.FormValue("repo"), r.FormValue("branch"), r.FormValue("workdir"), r.FormValue("auto_approve") == "on")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/projects/"+string(p.Snapshot().ID), http.StatusSeeOther)
}

func (u *ui) project(w http.ResponseWriter, r *http.Request) {
	p, runs, err := u.loadProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	tracked := filterRuns(runs, isTracked)
	var panel bytes.Buffer
	if err := pages.ExecuteTemplate(&panel, "tracked", trackedTab(ps.ID, tracked)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.page(w, ps.Name, "project", u.projectView(r, ps, runs, template.HTML(panel.String())))
}

func (u *ui) projectHeadline(w http.ResponseWriter, r *http.Request) {
	p, runs, err := u.loadProject(w, r)
	if err != nil {
		return
	}
	u.fragment(w, "project_headline", u.projectView(r, p.Snapshot(), runs, ""))
}

func (u *ui) tabTracked(w http.ResponseWriter, r *http.Request) {
	p, runs, err := u.loadProject(w, r)
	if err != nil {
		return
	}
	u.fragment(w, "tracked", trackedTab(p.Snapshot().ID, filterRuns(runs, isTracked)))
}

func (u *ui) tabPulls(w http.ResponseWriter, r *http.Request) {
	p, runs, err := u.loadProject(w, r)
	if err != nil {
		return
	}
	u.fragment(w, "pulls", pullsTab(p.Snapshot().ID, filterRuns(runs, isPR)))
}

func (u *ui) tabState(w http.ResponseWriter, r *http.Request) {
	p, runs, err := u.loadProject(w, r)
	if err != nil {
		return
	}
	hist, err := u.cfg.States.History(r.Context(), string(p.Snapshot().ID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.fragment(w, "state_history", stateTab(p.Snapshot().ID, hist, anyNonTerminal(runs)))
}

func (u *ui) details(w http.ResponseWriter, r *http.Request) {
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	u.page(w, ps.Name+" details", "details", map[string]any{
		"ID": string(ps.ID), "Name": ps.Name, "WebhookURL": u.webhookURL(ps.ID),
		"Secret": ps.WebhookSecret, "AutoApprove": ps.AutoApprove,
		"Branch": ps.DefaultBranch, "Workdir": ps.WorkingDirectory,
	})
}

func (u *ui) update(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	id := p.Snapshot().ID
	if _, err := u.cfg.Projects.Update(r.Context(), projectapp.UpdateInput{
		ID: id, Name: p.Snapshot().Name, Branch: r.FormValue("branch"), Workdir: r.FormValue("workdir"),
		Token: r.FormValue("token"), AutoApprove: r.FormValue("auto_approve") == "on", RotateSecret: r.FormValue("rotate_secret") == "on",
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/projects/"+string(id)+"/details", http.StatusSeeOther)
}

func (u *ui) lock(w http.ResponseWriter, r *http.Request) {
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	if err := u.cfg.Projects.SetLocked(r.Context(), p.Snapshot().ID, !p.Snapshot().Locked); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+string(p.Snapshot().ID), http.StatusSeeOther)
}

func (u *ui) trigger(w http.ResponseWriter, r *http.Request) {
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	sha, err := u.cfg.Checkout.BranchTip(r.Context(), ps.Repo.CloneURL(), ps.GitHubToken, ps.DefaultBranch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	msg, author, err := u.cfg.Lookup.Lookup(r.Context(), ps.Repo, ps.GitHubToken, sha)
	if err != nil {
		msg, author = "", ""
	}
	opened, err := u.cfg.Runs.Open(r.Context(), runapp.OpenRequest{
		ProjectID: ps.ID, Trigger: run.TriggerManual, SHA: sha, Branch: ps.DefaultBranch,
		Message: msg, Author: author, TriggeredBy: u.actor(r), PlanOnly: false,
	})
	if err != nil {
		if errors.Is(err, runapp.ErrDuplicate) {
			http.Redirect(w, r, "/projects/"+string(ps.ID), http.StatusSeeOther)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/runs/"+string(opened.Snapshot().ID), http.StatusSeeOther)
}

func (u *ui) forceUnlock(w http.ResponseWriter, r *http.Request) {
	id := project.ID(r.PathValue("id"))
	if err := u.cfg.Projects.ForceUnlock(r.Context(), id, u.cfg.States.States); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/projects/"+string(id)+"/details", http.StatusSeeOther)
}

func (u *ui) delete(w http.ResponseWriter, r *http.Request) {
	if err := u.cfg.Projects.Delete(r.Context(), project.ID(r.PathValue("id"))); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (u *ui) runPage(w http.ResponseWriter, r *http.Request) {
	rn, p, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	rs := rn.Snapshot()
	var panel bytes.Buffer
	if err := pages.ExecuteTemplate(&panel, "history", historyFrom(rs)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.page(w, runTitle(rs), "run", u.runView(r, p.Snapshot(), rs, template.HTML(panel.String())))
}

func (u *ui) runHeadline(w http.ResponseWriter, r *http.Request) {
	rn, p, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	u.fragment(w, "run_headline", u.runView(r, p.Snapshot(), rn.Snapshot(), ""))
}

func (u *ui) history(w http.ResponseWriter, r *http.Request) {
	rn, _, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	u.fragment(w, "history", historyFrom(rn.Snapshot()))
}

func (u *ui) changes(w http.ResponseWriter, r *http.Request) {
	rn, _, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	s := rn.Snapshot()
	u.fragment(w, "changes", map[string]any{
		"Delta": template.HTML(delta(s)), "PlanText": s.PlanText,
	})
}

func (u *ui) confirm(w http.ResponseWriter, r *http.Request) {
	id := run.ID(r.PathValue("id"))
	if err := u.cfg.Runs.Confirm(r.Context(), id, u.actor(r)); err != nil {
		if errors.Is(err, run.ErrActiveConflict) {
			http.Error(w, "Another run for this commit is queued or running; it must finish before this run can be confirmed.", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		u.history(w, r)
		return
	}
	http.Redirect(w, r, "/runs/"+string(id), http.StatusSeeOther)
}

func (u *ui) cancel(w http.ResponseWriter, r *http.Request) {
	id := run.ID(r.PathValue("id"))
	if err := u.cfg.Runs.Cancel(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		u.history(w, r)
		return
	}
	http.Redirect(w, r, "/runs/"+string(id), http.StatusSeeOther)
}

func (u *ui) getProject(w http.ResponseWriter, r *http.Request) (*project.Project, error) {
	p, err := u.cfg.Projects.Get(r.Context(), project.ID(r.PathValue("id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, err
	}
	if p == nil {
		http.NotFound(w, r)
		return nil, fmt.Errorf("not found")
	}
	return p, nil
}

func (u *ui) loadProject(w http.ResponseWriter, r *http.Request) (*project.Project, []*run.Run, error) {
	p, err := u.getProject(w, r)
	if err != nil {
		return nil, nil, err
	}
	runs, err := u.cfg.Runs.ListByProject(r.Context(), p.Snapshot().ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil, err
	}
	return p, runs, nil
}

func (u *ui) loadRun(w http.ResponseWriter, r *http.Request) (*run.Run, *project.Project, error) {
	rn, err := u.cfg.Runs.Get(r.Context(), run.ID(r.PathValue("id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil, err
	}
	if rn == nil {
		http.NotFound(w, r)
		return nil, nil, fmt.Errorf("not found")
	}
	p, err := u.cfg.Projects.Get(r.Context(), project.ID(rn.Snapshot().ProjectID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil, err
	}
	if p == nil {
		http.NotFound(w, r)
		return nil, nil, fmt.Errorf("not found")
	}
	return rn, p, nil
}

func (u *ui) webhookURL(id project.ID) string {
	return strings.TrimRight(u.cfg.PublicBaseURL, "/") + "/webhooks/github/" + string(id)
}

func (u *ui) projectView(r *http.Request, ps project.Snapshot, runs []*run.Run, panel template.HTML) map[string]any {
	tracked := filterRuns(runs, isTracked)
	latest := latestSnapshot(tracked)
	hist, _ := u.cfg.States.History(r.Context(), string(ps.ID))
	return map[string]any{
		"ID": string(ps.ID), "Name": ps.Name, "Locked": ps.Locked,
		"LatestLabel": latestLabel(latest), "LatestClass": latestClass(latest),
		"Meta": projectMeta(ps, latest, hist), "Empty": len(runs) == 0,
		"WebhookURL": u.webhookURL(ps.ID), "Secret": ps.WebhookSecret, "Panel": panel,
		"Poll": anyNonTerminal(runs),
	}
}

func (u *ui) runView(r *http.Request, ps project.Snapshot, rs run.Snapshot, panel template.HTML) map[string]any {
	hist, _ := u.cfg.States.History(r.Context(), string(ps.ID))
	return map[string]any{
		"ID": string(rs.ID), "ProjectID": string(ps.ID), "ProjectName": ps.Name,
		"Title": runTitle(rs), "Status": rs.Status.Label(), "StatusClass": statusClass(rs.Status),
		"Meta": runMeta(ps, rs, hist), "ChangesLabel": template.HTML(changesLabel(rs)), "Panel": panel,
		"Poll": !rs.Status.Terminal(),
	}
}

func projectMeta(ps project.Snapshot, latest *run.Snapshot, hist []state.HistoryEntry) metaView {
	m := metaView{Repo: ps.Repo.String(), Workdir: ps.WorkingDirectory, Branch: ps.DefaultBranch}
	if latest != nil {
		m.TerraformVersion = latest.TerraformVersion
		m.SHA = shortSHA(latest.SHA)
		if latest.Branch != "" {
			m.Branch = latest.Branch
		}
	}
	if n := len(hist); n > 0 {
		m.StateUpdated = fmtTime(hist[n-1].CreatedAt)
	}
	return m
}

func runMeta(ps project.Snapshot, rs run.Snapshot, hist []state.HistoryEntry) metaView {
	m := metaView{
		TerraformVersion: rs.TerraformVersion, Repo: ps.Repo.String(), Workdir: ps.WorkingDirectory,
		Branch: firstNonEmpty(rs.Branch, ps.DefaultBranch), SHA: shortSHA(rs.SHA),
	}
	if n := len(hist); n > 0 {
		m.StateUpdated = fmtTime(hist[n-1].CreatedAt)
	}
	return m
}

func trackedTab(id project.ID, runs []*run.Run) tabView {
	return tabView{
		ProjectID: string(id), Poll: anyNonTerminal(runs),
		Groups: groupRuns(runs, func(s run.Snapshot) string { return s.SHA }, func(s run.Snapshot) string { return firstLine(s.Message) }),
	}
}

func pullsTab(id project.ID, runs []*run.Run) tabView {
	return tabView{
		ProjectID: string(id), Poll: anyNonTerminal(runs),
		Groups: groupRuns(runs, prKey, func(s run.Snapshot) string { return s.PRTitle }),
	}
}

func stateTab(id project.ID, hist []state.HistoryEntry, poll bool) tabView {
	entries := make([]stateRow, 0, len(hist))
	for i := len(hist) - 1; i >= 0; i-- {
		h := hist[i]
		serial := ""
		if h.Serial != nil {
			serial = strconv.FormatInt(*h.Serial, 10)
		}
		entries = append(entries, stateRow{
			Time: fmtTime(h.CreatedAt), RunID: h.RunID, Serial: serial,
			Size: fmt.Sprintf("%d B", len(h.Data)), JSON: string(h.Data),
		})
	}
	return tabView{ProjectID: string(id), Poll: poll, Entries: entries}
}

func groupRuns(runs []*run.Run, keyFn, titleFn func(run.Snapshot) string) []runGroup {
	type acc struct {
		title, author, sha string
		at                 time.Time
		rows               []runRow
	}
	seen := map[string]*acc{}
	var keys []string
	for _, r := range runs {
		s := r.Snapshot()
		k := keyFn(s)
		a, ok := seen[k]
		if !ok {
			a = &acc{title: titleFn(s), author: s.Author, sha: shortSHA(s.SHA), at: s.CreatedAt}
			seen[k] = a
			keys = append(keys, k)
		}
		if s.CreatedAt.After(a.at) {
			a.at = s.CreatedAt
			a.title = titleFn(s)
			if s.Author != "" {
				a.author = s.Author
			}
			a.sha = shortSHA(s.SHA)
		}
		a.rows = append(a.rows, toRunRow(s))
	}
	sort.Slice(keys, func(i, j int) bool { return seen[keys[i]].at.After(seen[keys[j]].at) })
	out := make([]runGroup, 0, len(keys))
	for _, k := range keys {
		a := seen[k]
		sort.Slice(a.rows, func(i, j int) bool { return a.rows[i].sortAt.After(a.rows[j].sortAt) })
		out = append(out, runGroup{Title: a.title, Author: a.author, Date: fmtTime(a.at), SHA: a.sha, Runs: a.rows})
	}
	return out
}

func toRunRow(s run.Snapshot) runRow {
	triggered := s.TriggeredBy
	if triggered == "" {
		triggered = string(s.Trigger)
	}
	return runRow{
		ID: string(s.ID), Status: s.Status.Label(), StatusClass: statusClass(s.Status),
		Date: fmtTime(s.CreatedAt), TriggeredBy: triggered, TerraformVersion: s.TerraformVersion,
		Delta: template.HTML(delta(s)), sortAt: s.CreatedAt,
	}
}

func historyFrom(s run.Snapshot) historyView {
	h := historyView{
		ID: string(s.ID), Poll: !s.Status.Terminal(), StatusLabel: s.Status.Label(), StatusClass: statusClass(s.Status),
		Terminal: s.Status.Terminal(), Unconfirmed: s.Status == run.StatusNeedsApproval,
		Confirmed: s.ApprovedBy != "", ApprovedBy: s.ApprovedBy, Queued: true, QueuedAt: fmtTime(s.CreatedAt),
	}
	for _, st := range s.Steps {
		sv := &stepView{Open: st.Status == run.StepRunning, Logs: st.Logs}
		switch st.Name {
		case run.StepInit:
			if st.Status != run.StepPending {
				h.Init = sv
			}
		case run.StepPlan:
			if st.Status != run.StepPending {
				h.Plan = sv
			}
		case run.StepApply:
			if st.Status != run.StepPending {
				h.Apply = sv
			}
		}
	}
	return h
}

func filterRuns(runs []*run.Run, ok func(run.Snapshot) bool) []*run.Run {
	var out []*run.Run
	for _, r := range runs {
		if ok(r.Snapshot()) {
			out = append(out, r)
		}
	}
	return out
}

func isTracked(s run.Snapshot) bool {
	return s.Trigger == run.TriggerPush || s.Trigger == run.TriggerManual
}

func isPR(s run.Snapshot) bool { return s.Trigger == run.TriggerPullRequest }

func anyNonTerminal(runs []*run.Run) bool {
	for _, r := range runs {
		if !r.Snapshot().Status.Terminal() {
			return true
		}
	}
	return false
}

func latestSnapshot(runs []*run.Run) *run.Snapshot {
	var best *run.Snapshot
	for _, r := range runs {
		s := r.Snapshot()
		if best == nil || s.CreatedAt.After(best.CreatedAt) {
			cp := s
			best = &cp
		}
	}
	return best
}

func latestLabel(s *run.Snapshot) string {
	if s == nil {
		return ""
	}
	return s.Status.Label()
}

func latestClass(s *run.Snapshot) string {
	if s == nil {
		return ""
	}
	return statusClass(s.Status)
}

func statusClass(st run.Status) string {
	switch st {
	case run.StatusSucceeded:
		return "pill-finished"
	case run.StatusFailed:
		return "pill-failed"
	case run.StatusNeedsApproval:
		return "pill-unconfirmed"
	case run.StatusPlanned:
		return "pill-planned"
	case run.StatusCancelled:
		return "pill-cancelled"
	default:
		return "pill"
	}
}

func runTitle(s run.Snapshot) string {
	if s.Trigger == run.TriggerPullRequest && s.PRTitle != "" {
		return s.PRTitle
	}
	if t := firstLine(s.Message); t != "" {
		return t
	}
	return shortSHA(s.SHA)
}

func changesLabel(s run.Snapshot) string {
	if s.SummaryPresent {
		return fmt.Sprintf("Changes +%d", s.Add)
	}
	return "Changes"
}

func delta(s run.Snapshot) string {
	if !s.SummaryPresent {
		return ""
	}
	return fmt.Sprintf("+%d ~%d \u2212%d", s.Add, s.Change, s.Destroy)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func prKey(s run.Snapshot) string {
	if s.PRNumber != nil {
		return strconv.Itoa(*s.PRNumber)
	}
	return string(s.ID)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04")
}
