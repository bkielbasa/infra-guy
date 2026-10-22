package httpapi

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"infra-guy/internal/application/installation"
	policyapp "infra-guy/internal/application/policy"
	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	tfcontextapp "infra-guy/internal/application/tfcontext"
	"infra-guy/internal/domain/policy"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/state"
	"infra-guy/internal/domain/tfcontext"
	"infra-guy/internal/infrastructure/terraform"
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
	Open    bool
	Running bool
	Status  string
	Logs    string
}

type runPolicyEvalView struct {
	PolicyID   string
	PolicyName string
	Status     string
	Allowed    bool
	Undecided  bool
	Deny       []string
	Error      string
}

type historyView struct {
	ID, StatusLabel, StatusClass, ApprovedBy, QueuedAt string
	ApprovedByList                                     string
	ApprovalCount                                      int
	CanApprove, PlanOnly                               bool
	Poll, Terminal, Unconfirmed, Confirmed, Cancelled  bool
	Queued                                             bool
	Apply, Plan, Init                                  *stepView
	PlanDiff                                           *run.ParsedPlan
	PolicyEval                                         *runPolicyEvalView
}

func (u *ui) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /github/login", u.githubLogin)
	mux.HandleFunc("GET /github/callback", u.githubCallback)
	mux.HandleFunc("GET /github/setup", u.githubSetup)
	mux.HandleFunc("POST /github/tenant", u.githubTenant)
	mux.HandleFunc("GET /{$}", u.list)
	mux.HandleFunc("GET /projects", u.list)
	mux.HandleFunc("GET /projects/new", u.newProject)
	mux.HandleFunc("POST /projects", u.create)
	mux.HandleFunc("GET /projects/{id}", u.project)
	mux.HandleFunc("GET /projects/{id}/headline", u.projectHeadline)
	mux.HandleFunc("GET /projects/{id}/tabs/tracked", u.tabTracked)
	mux.HandleFunc("GET /projects/{id}/tabs/pulls", u.tabPulls)
	mux.HandleFunc("GET /projects/{id}/tabs/state", u.tabState)
	mux.HandleFunc("GET /projects/{id}/details", u.details)
	mux.HandleFunc("GET /projects/{id}/settings", u.details)
	mux.HandleFunc("POST /projects/{id}", u.update)
	mux.HandleFunc("POST /projects/{id}/policy", u.updateProjectPolicy)
	mux.HandleFunc("POST /projects/{id}/lock", u.lock)
	mux.HandleFunc("POST /projects/{id}/trigger", u.trigger)
	mux.HandleFunc("POST /projects/{id}/force-unlock", u.forceUnlock)
	mux.HandleFunc("POST /projects/{id}/delete", u.delete)
	mux.HandleFunc("GET /projects/{id}/runs/{runID}", u.runPage)
	mux.HandleFunc("POST /projects/{id}/runs/{runID}/approve", u.approve)
	mux.HandleFunc("POST /projects/{id}/runs/{runID}/confirm", u.confirm)
	mux.HandleFunc("POST /projects/{id}/runs/{runID}/cancel", u.cancel)
	mux.HandleFunc("GET /runs/{id}", u.runPage)
	mux.HandleFunc("GET /runs/{id}/headline", u.runHeadline)
	mux.HandleFunc("GET /runs/{id}/history", u.history)
	mux.HandleFunc("GET /runs/{id}/changes", u.changes)
	mux.HandleFunc("POST /runs/{id}/approve", u.approve)
	mux.HandleFunc("POST /runs/{id}/confirm", u.confirm)
	mux.HandleFunc("POST /runs/{id}/cancel", u.cancel)
	mux.HandleFunc("GET /contexts", u.contexts)
	mux.HandleFunc("POST /contexts", u.createContext)
	mux.HandleFunc("GET /contexts/{id}", u.contextDetails)
	mux.HandleFunc("POST /contexts/{id}/entries", u.upsertContextEntry)
	mux.HandleFunc("POST /contexts/{id}/entries/{entryID}/delete", u.deleteContextEntry)
	mux.HandleFunc("POST /contexts/{id}/delete", u.deleteContext)
	mux.HandleFunc("GET /policies", u.policies)
	mux.HandleFunc("POST /policies", u.createPolicy)
	mux.HandleFunc("POST /policies/test", u.testPolicy)
	mux.HandleFunc("GET /policies/{id}", u.policyDetails)
	mux.HandleFunc("POST /policies/{id}", u.updatePolicy)
	mux.HandleFunc("POST /policies/{id}/delete", u.deletePolicy)
	mux.Handle("GET /static/", http.FileServer(http.FS(content)))
}

func (u *ui) actor(r *http.Request) string {
	login, _, _, _ := SessionFrom(r.Context())
	return login
}

func (u *ui) now() time.Time {
	if u.cfg.Auth != nil && u.cfg.Auth.Now != nil {
		return u.cfg.Auth.Now()
	}
	return time.Now().UTC()
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
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if tenantID == "" {
		u.picker(w, r)
		return
	}
	items, err := u.cfg.Projects.List(r.Context(), tenantID)
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
	u.page(w, "Projects", "projects", map[string]any{
		"Projects": rows,
		"Policies": u.policyChoices(r.Context(), tenantID, ""),
	})
}

func (u *ui) newProject(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.Installations == nil {
		u.page(w, "New project", "new_project", nil)
		return
	}
	repos, err := u.cfg.Installations.Repos(r.Context(), tenantID)
	if err != nil {
		if errors.Is(err, installation.ErrNotInstalled) {
			var installURL string
			if u.cfg.Auth != nil {
				installURL, _ = u.cfg.Auth.InstallURL(r.Context())
			}
			u.page(w, "New project", "new_project", map[string]any{"InstallURL": installURL})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.page(w, "New project", "new_project", map[string]any{
		"Repos":          repos,
		"Versions":       u.versions(r.Context()),
		"DefaultVersion": project.DefaultTerraformVersion,
		"Contexts":       u.contextChoices(r.Context(), tenantID, ""),
		"Policies":       u.policyChoices(r.Context(), tenantID, ""),
	})
}

func (u *ui) create(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	policyID := strings.TrimSpace(r.FormValue("approval_policy_id"))
	if policyID != "" && u.cfg.Policies != nil {
		if _, err := u.cfg.Policies.Get(r.Context(), tenantID, policy.ID(policyID)); err != nil {
			http.Error(w, "invalid approval_policy_id", http.StatusBadRequest)
			return
		}
	}
	rawRepoID := r.FormValue("repo_id")
	repoID, err := strconv.ParseInt(rawRepoID, 10, 64)
	if err != nil || repoID <= 0 {
		http.Error(w, "invalid repo_id", http.StatusBadRequest)
		return
	}
	if u.cfg.Installations == nil {
		http.Error(w, "installations service not configured", http.StatusInternalServerError)
		return
	}
	repos, err := u.cfg.Installations.Repos(r.Context(), tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var match *installation.Repo
	for i := range repos {
		if repos[i].ID == repoID {
			match = &repos[i]
			break
		}
	}
	if match == nil {
		http.Error(w, "repository not found", http.StatusBadRequest)
		return
	}
	sourceRepo := project.SourceRepo{Owner: match.Owner, Name: match.Name}
	p, err := u.cfg.Projects.Create(r.Context(), tenantID, r.FormValue("name"), sourceRepo, match.ID, r.FormValue("branch"), r.FormValue("workdir"), r.FormValue("auto_approve") == "on")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := u.cfg.Projects.SetTerraformVersion(r.Context(), p.Snapshot().ID, r.FormValue("terraform_version")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if policyID != "" {
		if err := u.cfg.Projects.UpdateApprovalPolicy(r.Context(), p.Snapshot().ID, policyID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if u.cfg.Contexts != nil {
		if err := u.cfg.Contexts.SetProjectContexts(r.Context(), tenantID, p.Snapshot().ID, parseContextIDs(r.Form["context_ids"])); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
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
		"ID": string(ps.ID), "Name": ps.Name, "AutoApprove": ps.AutoApprove,
		"Branch": ps.DefaultBranch, "Workdir": ps.WorkingDirectory,
		"TerraformVersion": ps.TerraformVersion,
		"ApprovalPolicyID": ps.ApprovalPolicyID,
		"Versions":         u.versions(r.Context()),
		"Contexts":         u.contextChoices(r.Context(), ps.TenantID, ps.ID),
		"Policies":         u.policyChoices(r.Context(), ps.TenantID, policy.ID(ps.ApprovalPolicyID)),
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
	ps := p.Snapshot()
	id := ps.ID
	policyID := strings.TrimSpace(r.FormValue("approval_policy_id"))
	if policyID != "" && u.cfg.Policies != nil {
		if _, err := u.cfg.Policies.Get(r.Context(), ps.TenantID, policy.ID(policyID)); err != nil {
			http.Error(w, "invalid approval_policy_id", http.StatusBadRequest)
			return
		}
	}
	if _, err := u.cfg.Projects.Update(r.Context(), projectapp.UpdateInput{
		ID: id, Name: ps.Name, Branch: r.FormValue("branch"), Workdir: r.FormValue("workdir"),
		TerraformVersion: r.FormValue("terraform_version"),
		AutoApprove:      r.FormValue("auto_approve") == "on",
		ApprovalPolicyID: policyID,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := u.cfg.Projects.UpdateApprovalPolicy(r.Context(), id, policyID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if u.cfg.Contexts != nil {
		if err := u.cfg.Contexts.SetProjectContexts(r.Context(), ps.TenantID, id, parseContextIDs(r.Form["context_ids"])); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	http.Redirect(w, r, "/projects/"+string(id)+"/details", http.StatusSeeOther)
}

func (u *ui) updateProjectPolicy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	policyID := strings.TrimSpace(r.FormValue("approval_policy_id"))
	if policyID != "" && u.cfg.Policies != nil {
		if _, err := u.cfg.Policies.Get(r.Context(), ps.TenantID, policy.ID(policyID)); err != nil {
			http.Error(w, "invalid approval_policy_id", http.StatusBadRequest)
			return
		}
	}
	if err := u.cfg.Projects.UpdateApprovalPolicy(r.Context(), ps.ID, policyID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/projects/"+string(ps.ID), http.StatusSeeOther)
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
	if u.cfg.Tokens == nil {
		http.Error(w, "GitHub App is not installed", http.StatusBadGateway)
		return
	}
	token, err := u.cfg.Tokens.InstallationToken(r.Context(), ps.TenantID)
	if err != nil {
		http.Error(w, "GitHub App is not installed", http.StatusBadGateway)
		return
	}
	sha, err := u.cfg.Checkout.BranchTip(r.Context(), ps.Repo.CloneURL(), token, ps.DefaultBranch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	msg, author, err := u.cfg.Lookup.Lookup(r.Context(), ps.Repo, token, sha)
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
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	id := p.Snapshot().ID
	if err := u.cfg.Projects.ForceUnlock(r.Context(), id, u.cfg.States.States); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/projects/"+string(id)+"/details", http.StatusSeeOther)
}

func (u *ui) delete(w http.ResponseWriter, r *http.Request) {
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	if err := u.cfg.Projects.Delete(r.Context(), p.Snapshot().ID); err != nil {
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
	ps := p.Snapshot()
	evalView := u.evaluateRunPolicy(r, ps, rs)
	var panel bytes.Buffer
	if err := pages.ExecuteTemplate(&panel, "history", historyFrom(rs, evalView, u.actor(r))); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.page(w, runTitle(rs), "run", u.runView(r, ps, rs, evalView, template.HTML(panel.String())))
}

func (u *ui) runHeadline(w http.ResponseWriter, r *http.Request) {
	rn, p, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	rs := rn.Snapshot()
	ps := p.Snapshot()
	evalView := u.evaluateRunPolicy(r, ps, rs)
	u.fragment(w, "run_headline", u.runView(r, ps, rs, evalView, ""))
}

func (u *ui) history(w http.ResponseWriter, r *http.Request) {
	rn, p, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	rs := rn.Snapshot()
	ps := p.Snapshot()
	evalView := u.evaluateRunPolicy(r, ps, rs)
	u.fragment(w, "history", historyFrom(rs, evalView, u.actor(r)))
}

func (u *ui) changes(w http.ResponseWriter, r *http.Request) {
	rn, _, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	s := rn.Snapshot()
	var planDiff *run.ParsedPlan
	if len(s.PlanJSON) > 0 {
		planDiff, _ = run.ParsePlanJSON(s.PlanJSON)
	}
	u.fragment(w, "changes", map[string]any{
		"Delta": template.HTML(delta(s)), "PlanText": s.PlanText, "PlanDiff": planDiff,
	})
}

func (u *ui) approve(w http.ResponseWriter, r *http.Request) {
	rn, _, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	id := rn.Snapshot().ID
	info := u.userInfo(r)
	if err := u.cfg.Runs.Approve(r.Context(), id, run.Approval{
		UserID: info.ID,
		Login:  firstNonEmpty(info.Login, u.actor(r)),
		Role:   info.Role,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/runs/"+string(id))
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/runs/"+string(id), http.StatusSeeOther)
}

func (u *ui) confirm(w http.ResponseWriter, r *http.Request) {
	rn, p, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	rs := rn.Snapshot()
	ps := p.Snapshot()
	if ps.ApprovalPolicyID != "" {
		if u.cfg.Policies == nil {
			http.Error(w, "policy service not configured", http.StatusInternalServerError)
			return
		}
		res, err := u.cfg.Policies.EvaluateForRun(r.Context(), ps.TenantID, policy.ID(ps.ApprovalPolicyID), rs.PlanJSON, u.userInfo(r), projectInfoFrom(ps), approvalsInfoFrom(rs.Approvals))
		if err != nil {
			http.Error(w, "Policy evaluation failed: "+err.Error(), http.StatusForbidden)
			return
		}
		if !res.Allowed {
			msg := "Approval denied by policy"
			if res.Undecided {
				msg = "Approval undecided by policy"
			}
			if len(res.Deny) > 0 {
				msg += ": " + strings.Join(res.Deny, "; ")
			}
			http.Error(w, msg, http.StatusForbidden)
			return
		}
	}
	id := rs.ID
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
	rn, _, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	id := rn.Snapshot().ID
	if err := u.cfg.Runs.Cancel(r.Context(), id, u.actor(r)); err != nil {
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
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || p.Snapshot().TenantID != tenantID {
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

func (u *ui) runID(r *http.Request) run.ID {
	if rid := r.PathValue("runID"); rid != "" {
		return run.ID(rid)
	}
	return run.ID(r.PathValue("id"))
}

func (u *ui) loadRun(w http.ResponseWriter, r *http.Request) (*run.Run, *project.Project, error) {
	rn, err := u.cfg.Runs.Get(r.Context(), u.runID(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil, err
	}
	if rn == nil {
		http.NotFound(w, r)
		return nil, nil, fmt.Errorf("not found")
	}
	if r.PathValue("runID") != "" && r.PathValue("id") != "" && string(rn.Snapshot().ProjectID) != r.PathValue("id") {
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
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || p.Snapshot().TenantID != tenantID {
		http.NotFound(w, r)
		return nil, nil, fmt.Errorf("not found")
	}
	return rn, p, nil
}

func (u *ui) projectView(r *http.Request, ps project.Snapshot, runs []*run.Run, panel template.HTML) map[string]any {
	tracked := filterRuns(runs, isTracked)
	latest := latestSnapshot(tracked)
	hist, _ := u.cfg.States.History(r.Context(), string(ps.ID))
	return map[string]any{
		"ID": string(ps.ID), "Name": ps.Name, "Locked": ps.Locked,
		"LatestLabel": latestLabel(latest), "LatestClass": latestClass(latest),
		"Meta": projectMeta(ps, latest, hist), "Empty": len(runs) == 0,
		"Panel": panel,
		"Poll":  anyNonTerminal(runs),
	}
}

func (u *ui) runView(r *http.Request, ps project.Snapshot, rs run.Snapshot, evalView *runPolicyEvalView, panel template.HTML) map[string]any {
	hist, _ := u.cfg.States.History(r.Context(), string(ps.ID))
	return map[string]any{
		"ID": string(rs.ID), "ProjectID": string(ps.ID), "ProjectName": ps.Name,
		"Title": runTitle(rs), "Status": rs.Status.Label(), "StatusClass": statusClass(rs.Status),
		"Meta": runMeta(ps, rs, hist), "ChangesLabel": template.HTML(changesLabel(rs)), "Panel": panel,
		"Poll":       !rs.Status.Terminal(),
		"PolicyEval": evalView,
	}
}

func projectMeta(ps project.Snapshot, latest *run.Snapshot, hist []state.HistoryEntry) metaView {
	m := metaView{
		TerraformVersion: ps.TerraformVersion,
		Repo:             ps.Repo.String(),
		Workdir:          ps.WorkingDirectory,
		Branch:           ps.DefaultBranch,
	}
	if latest != nil {
		if latest.TerraformVersion != "" {
			m.TerraformVersion = latest.TerraformVersion
		}
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

func historyFrom(s run.Snapshot, evalView *runPolicyEvalView, currentLogin string) historyView {
	approvers := make([]string, 0, len(s.Approvals))
	alreadyApproved := false
	for _, a := range s.Approvals {
		approvers = append(approvers, a.Login)
		if currentLogin != "" && strings.EqualFold(a.Login, currentLogin) {
			alreadyApproved = true
		}
	}
	var planDiff *run.ParsedPlan
	if len(s.PlanJSON) > 0 {
		planDiff, _ = run.ParsePlanJSON(s.PlanJSON)
	}
	approvable := s.Status == run.StatusNeedsApproval || (s.PlanOnly && s.Status == run.StatusPlanned && s.HasChanges())
	h := historyView{
		ID: string(s.ID), Poll: !s.Status.Terminal(), StatusLabel: s.Status.Label(), StatusClass: statusClass(s.Status),
		Terminal:       s.Status.Terminal(),
		Unconfirmed:    approvable,
		PlanOnly:       s.PlanOnly,
		Confirmed:      s.ApprovedBy != "" && s.Status != run.StatusCancelled,
		Cancelled:      s.Status == run.StatusCancelled,
		ApprovedBy:     s.ApprovedBy,
		ApprovedByList: strings.Join(approvers, ", "),
		ApprovalCount:  len(s.Approvals),
		CanApprove:     approvable && !alreadyApproved,
		Queued:         true,
		QueuedAt:       fmtTime(s.CreatedAt),
		PlanDiff:       planDiff,
		PolicyEval:     evalView,
	}
	for _, st := range s.Steps {
		open := st.Status == run.StepRunning ||
			st.Status == run.StepFailed ||
			(st.Name == run.StepPlan && (s.Status == run.StatusNeedsApproval || s.Status == run.StatusPlanned))
		sv := &stepView{
			Open:    open,
			Running: st.Status == run.StepRunning,
			Status:  string(st.Status),
			Logs:    st.Logs,
		}
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

type contextChoice struct {
	ID       string
	Name     string
	Attached bool
}

type contextRow struct {
	ID         string
	Name       string
	EntryCount int
	UpdatedAt  string
}

type contextEntryRow struct {
	ID        string
	Kind      string
	Key       string
	Value     string
	Secret    bool
	UpdatedAt string
}

func (u *ui) versions(ctx context.Context) []string {
	if u.cfg.Versions != nil {
		if list := u.cfg.Versions.List(ctx); len(list) > 0 {
			return list
		}
	}
	return terraform.FallbackVersions
}

func (u *ui) contextChoices(ctx context.Context, tenantID string, projectID project.ID) []contextChoice {
	if u.cfg.Contexts == nil || tenantID == "" {
		return nil
	}
	all, err := u.cfg.Contexts.List(ctx, tenantID)
	if err != nil {
		return nil
	}
	attached := make(map[tfcontext.ID]bool)
	if projectID != "" {
		if list, err := u.cfg.Contexts.ListByProject(ctx, projectID); err == nil {
			for _, c := range list {
				attached[c.Snapshot().ID] = true
			}
		}
	}
	out := make([]contextChoice, 0, len(all))
	for _, c := range all {
		cs := c.Snapshot()
		out = append(out, contextChoice{
			ID:       string(cs.ID),
			Name:     cs.Name,
			Attached: attached[cs.ID],
		})
	}
	return out
}

func parseContextIDs(raw []string) []tfcontext.ID {
	out := make([]tfcontext.ID, 0, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id != "" {
			out = append(out, tfcontext.ID(id))
		}
	}
	return out
}

func (u *ui) contexts(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	var rows []contextRow
	if u.cfg.Contexts != nil {
		items, err := u.cfg.Contexts.List(r.Context(), tenantID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows = make([]contextRow, 0, len(items))
		for _, c := range items {
			cs := c.Snapshot()
			rows = append(rows, contextRow{
				ID:         string(cs.ID),
				Name:       cs.Name,
				EntryCount: len(cs.Entries),
				UpdatedAt:  fmtTime(cs.UpdatedAt),
			})
		}
	}
	u.page(w, "Contexts", "contexts", map[string]any{"Contexts": rows})
}

func (u *ui) createContext(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.Contexts == nil {
		http.Error(w, "contexts service not configured", http.StatusInternalServerError)
		return
	}
	c, err := u.cfg.Contexts.Create(r.Context(), tenantID, r.FormValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/contexts/"+string(c.Snapshot().ID), http.StatusSeeOther)
}

func (u *ui) contextDetails(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.Contexts == nil {
		http.NotFound(w, r)
		return
	}
	c, err := u.cfg.Contexts.Get(r.Context(), tenantID, tfcontext.ID(r.PathValue("id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	cs := c.Snapshot()
	entries := make([]contextEntryRow, 0, len(cs.Entries))
	for _, e := range cs.Entries {
		val := e.Value
		if e.Secret {
			val = ""
		}
		entries = append(entries, contextEntryRow{
			ID:        e.ID,
			Kind:      string(e.Kind),
			Key:       e.Key,
			Value:     val,
			Secret:    e.Secret,
			UpdatedAt: fmtTime(e.UpdatedAt),
		})
	}
	u.page(w, cs.Name, "context", map[string]any{
		"ID":      string(cs.ID),
		"Name":    cs.Name,
		"Entries": entries,
	})
}

func (u *ui) upsertContextEntry(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.Contexts == nil {
		http.Error(w, "contexts service not configured", http.StatusInternalServerError)
		return
	}
	id := tfcontext.ID(r.PathValue("id"))
	err := u.cfg.Contexts.UpsertEntry(
		r.Context(), tenantID, id,
		tfcontext.EntryKind(r.FormValue("kind")),
		r.FormValue("key"),
		r.FormValue("value"),
	)
	if err != nil {
		if errors.Is(err, tfcontextapp.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/contexts/"+string(id), http.StatusSeeOther)
}

func (u *ui) deleteContextEntry(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.Contexts == nil {
		http.Error(w, "contexts service not configured", http.StatusInternalServerError)
		return
	}
	id := tfcontext.ID(r.PathValue("id"))
	err := u.cfg.Contexts.DeleteEntry(r.Context(), tenantID, id, r.PathValue("entryID"))
	if err != nil {
		if errors.Is(err, tfcontextapp.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/contexts/"+string(id), http.StatusSeeOther)
}

func (u *ui) deleteContext(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.Contexts == nil {
		http.Error(w, "contexts service not configured", http.StatusInternalServerError)
		return
	}
	id := tfcontext.ID(r.PathValue("id"))
	if err := u.cfg.Contexts.Delete(r.Context(), tenantID, id); err != nil {
		if errors.Is(err, tfcontextapp.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/contexts", http.StatusSeeOther)
}

type policyChoice struct {
	ID       string
	Name     string
	Selected bool
}

type policyRow struct {
	ID          string
	Name        string
	Description string
	UpdatedAt   string
}

const defaultSampleRego = `package infraguy.approval

import rego.v1

allow if {
    count(input.approvals) > 2
}

deny contains "Destroying resources is not allowed without policy override" if {
    some rc in input.plan.resource_changes
    "delete" in rc.change.actions
}
`

const defaultSampleInputJSON = `{
  "plan": {
    "format_version": "1.2",
    "terraform_version": "1.8.5",
    "resource_changes": [
      {
        "address": "aws_s3_bucket.logs",
        "type": "aws_s3_bucket",
        "change": {
          "actions": ["create"]
        }
      }
    ]
  },
  "approvals": [
    {
      "id": "usr-1",
      "login": "alice",
      "role": "admin"
    }
  ],
  "current_user": {
    "id": "usr-1",
    "login": "alice",
    "role": "admin"
  },
  "project": {
    "id": "proj-1",
    "name": "production-vpc",
    "repo_owner": "acme",
    "repo_name": "infra",
    "working_directory": "envs/prod",
    "terraform_version": "1.8.5",
    "auto_approve": false
  }
}`

func (u *ui) policyChoices(ctx context.Context, tenantID string, selectedID policy.ID) []policyChoice {
	if u.cfg.Policies == nil || tenantID == "" {
		return nil
	}
	all, err := u.cfg.Policies.List(ctx, tenantID)
	if err != nil {
		return nil
	}
	out := make([]policyChoice, 0, len(all))
	for _, p := range all {
		ps := p.Snapshot()
		out = append(out, policyChoice{
			ID:       string(ps.ID),
			Name:     ps.Name,
			Selected: ps.ID == selectedID,
		})
	}
	return out
}

func (u *ui) policyName(ctx context.Context, tenantID string, policyID policy.ID) string {
	if u.cfg.Policies == nil || tenantID == "" || policyID == "" {
		return ""
	}
	p, err := u.cfg.Policies.Get(ctx, tenantID, policyID)
	if err != nil || p == nil {
		return ""
	}
	return p.Snapshot().Name
}

func (u *ui) userInfo(r *http.Request) policyapp.UserInfo {
	userID, login, _, role, ok := PrincipalFrom(r.Context())
	if !ok {
		return policyapp.UserInfo{Login: u.actor(r)}
	}
	return policyapp.UserInfo{ID: userID, Login: login, Role: role}
}

func projectInfoFrom(ps project.Snapshot) policyapp.ProjectInfo {
	return policyapp.ProjectInfo{
		ID:               string(ps.ID),
		Name:             ps.Name,
		RepoOwner:        ps.Repo.Owner,
		RepoName:         ps.Repo.Name,
		WorkingDirectory: ps.WorkingDirectory,
		TerraformVersion: ps.TerraformVersion,
		AutoApprove:      ps.AutoApprove,
	}
}

func approvalsInfoFrom(approvals []run.Approval) []policyapp.ApprovalInfo {
	out := make([]policyapp.ApprovalInfo, 0, len(approvals))
	for _, a := range approvals {
		var ts string
		if !a.ApprovedAt.IsZero() {
			ts = a.ApprovedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, policyapp.ApprovalInfo{
			ID:         a.UserID,
			Login:      a.Login,
			Role:       a.Role,
			ApprovedAt: ts,
		})
	}
	return out
}

func (u *ui) evaluateRunPolicy(r *http.Request, ps project.Snapshot, rs run.Snapshot) *runPolicyEvalView {
	approvable := rs.Status == run.StatusNeedsApproval || (rs.PlanOnly && rs.Status == run.StatusPlanned && rs.HasChanges())
	if !approvable || ps.ApprovalPolicyID == "" || u.cfg.Policies == nil {
		return nil
	}
	polID := policy.ID(ps.ApprovalPolicyID)
	polName := u.policyName(r.Context(), ps.TenantID, polID)
	if polName == "" {
		polName = ps.ApprovalPolicyID
	}
	res, err := u.cfg.Policies.EvaluateForRun(r.Context(), ps.TenantID, polID, rs.PlanJSON, u.userInfo(r), projectInfoFrom(ps), approvalsInfoFrom(rs.Approvals))
	if err != nil {
		return &runPolicyEvalView{
			PolicyID:   ps.ApprovalPolicyID,
			PolicyName: polName,
			Allowed:    false,
			Deny:       []string{"Policy evaluation error: " + err.Error()},
		}
	}
	return &runPolicyEvalView{
		PolicyID:   string(res.PolicyID),
		PolicyName: firstNonEmpty(res.PolicyName, polName),
		Status:     string(res.Status),
		Allowed:    res.Allowed,
		Undecided:  res.Undecided,
		Deny:       res.Deny,
	}
}

func (u *ui) policies(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	var rows []policyRow
	if u.cfg.Policies != nil {
		items, err := u.cfg.Policies.List(r.Context(), tenantID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows = make([]policyRow, 0, len(items))
		for _, p := range items {
			ps := p.Snapshot()
			rows = append(rows, policyRow{
				ID:          string(ps.ID),
				Name:        ps.Name,
				Description: ps.Description,
				UpdatedAt:   fmtTime(ps.UpdatedAt),
			})
		}
	}
	u.page(w, "Policies", "policies", map[string]any{
		"Policies":        rows,
		"DefaultRego":     defaultSampleRego,
		"SampleRego":      defaultSampleRego,
		"SampleInputJSON": defaultSampleInputJSON,
	})
}

func (u *ui) createPolicy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.Policies == nil {
		http.Error(w, "policies service not configured", http.StatusInternalServerError)
		return
	}
	p, err := u.cfg.Policies.Create(r.Context(), policyapp.CreateInput{
		TenantID:    tenantID,
		Name:        r.FormValue("name"),
		Description: r.FormValue("description"),
		Rego:        r.FormValue("rego"),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/policies/"+string(p.Snapshot().ID), http.StatusSeeOther)
}

func (u *ui) testPolicy(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.Policies == nil {
		http.Error(w, "policies service not configured", http.StatusInternalServerError)
		return
	}
	var regoSrc, inputStr string
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var payload struct {
			Rego      string          `json:"rego"`
			InputJSON string          `json:"input_json"`
			Input     json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid JSON request body: " + err.Error()})
			return
		}
		regoSrc = payload.Rego
		if payload.InputJSON != "" {
			inputStr = payload.InputJSON
		} else if len(payload.Input) > 0 {
			inputStr = string(payload.Input)
		}
	} else {
		if err := r.ParseForm(); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		regoSrc = r.FormValue("rego")
		inputStr = r.FormValue("input_json")
		if inputStr == "" {
			inputStr = r.FormValue("input")
		}
	}
	res, err := u.cfg.Policies.TestRego(r.Context(), regoSrc, []byte(inputStr))
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	deny := res.Deny
	if deny == nil {
		deny = []string{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    res.Status,
		"allowed":   res.Allowed,
		"undecided": res.Undecided,
		"deny":      deny,
	})
}

func (u *ui) policyDetails(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.Policies == nil {
		http.NotFound(w, r)
		return
	}
	p, err := u.cfg.Policies.Get(r.Context(), tenantID, policy.ID(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, policy.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.NotFound(w, r)
		return
	}
	ps := p.Snapshot()
	u.page(w, ps.Name, "policy", map[string]any{
		"ID":              string(ps.ID),
		"Name":            ps.Name,
		"Description":     ps.Description,
		"Rego":            ps.Rego,
		"UpdatedAt":       fmtTime(ps.UpdatedAt),
		"SampleInputJSON": defaultSampleInputJSON,
	})
}

func (u *ui) updatePolicy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.Policies == nil {
		http.Error(w, "policies service not configured", http.StatusInternalServerError)
		return
	}
	id := policy.ID(r.PathValue("id"))
	if _, err := u.cfg.Policies.Update(r.Context(), policyapp.UpdateInput{
		ID:          id,
		TenantID:    tenantID,
		Name:        r.FormValue("name"),
		Description: r.FormValue("description"),
		Rego:        r.FormValue("rego"),
	}); err != nil {
		if errors.Is(err, policy.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/policies/"+string(id), http.StatusSeeOther)
}

func (u *ui) deletePolicy(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.Policies == nil {
		http.Error(w, "policies service not configured", http.StatusInternalServerError)
		return
	}
	id := policy.ID(r.PathValue("id"))
	if err := u.cfg.Policies.Delete(r.Context(), tenantID, id); err != nil {
		if errors.Is(err, policy.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Cannot delete policy (detach it from any projects first): "+err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/policies", http.StatusSeeOther)
}

