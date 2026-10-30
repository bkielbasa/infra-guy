package httpapi

import (
	"bytes"
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	auditapp "infra-guy/internal/application/audit"
	cloudintegrationapp "infra-guy/internal/application/cloudintegration"
	folderapp "infra-guy/internal/application/folder"
	"infra-guy/internal/application/installation"
	policyapp "infra-guy/internal/application/policy"
	projectapp "infra-guy/internal/application/project"
	runapp "infra-guy/internal/application/run"
	tfcontextapp "infra-guy/internal/application/tfcontext"
	"infra-guy/internal/domain/audit"
	"infra-guy/internal/domain/cloudintegration"
	"infra-guy/internal/domain/dependency"
	"infra-guy/internal/domain/folder"
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
	Title     string
	Body      template.HTML
	UserLogin string
	UserRole  string
	TenantID  string
	AvatarURL string
}

type metaView struct {
	Engine, TerraformVersion, Repo, Workdir, Branch, SHA, SHAURL, PRNumber, PRURL, StateUpdated string
	Tags                                                                                        []string
	ShowManagedBy                                                                               bool
}

type projectRow struct {
	ID, Name, Repo, Branch, Status, StatusClass, StatusCategory string
	Tags                                                        []string
	TagsCSV                                                     string
	FolderID, FolderPath                                        string
	Drifted                                                     bool
	DriftEnabled                                                bool
	DriftStatus                                                 string
	DriftStatusLabel                                            string
	TTLHours                                                    int
	AutoDestroy                                                 bool
	Ephemeral                                                   bool
	ExpiresAt                                                   string
	TTLExpired                                                  bool
	Destroyed                                                   bool
	MonthlyCost                                                 string
	EstMonthlyCost                                              string
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
	ProjectID   string
	Poll        bool
	Groups      []runGroup
	State       *stateSummaryView
	Versions    []stateVersionView
	Diff        *stateDiffView
	CanRollback bool
	CompareFrom string
	CompareTo   string
}

type stateVersionView struct {
	Serial, Size, Resources, Actor, RunID, RunURL, When, Changed string
	Current                                                      bool
}

type stateChangeView struct {
	Name, Action, Before, After string
}

type stateDiffView struct {
	From, To                string
	Added, Changed, Removed int
	Unified                 string
	Resources               []stateChangeView
	Outputs                 []stateChangeView
}

type stateSummaryView struct {
	Time, Serial, Size string
	Locked             bool
}

type stepView struct {
	Open    bool
	Running bool
	Status  string
	Logs    string
}

type runPolicyEvalItemView struct {
	PolicyID    string
	PolicyName  string
	SourceBadge string
	Allowed     bool
	Undecided   bool
	Deny        []string
}

type runPolicyEvalView struct {
	PolicyID    string
	PolicyName  string
	SourceBadge string
	Status      string
	Allowed     bool
	Undecided   bool
	Deny        []string
	Error       string
	Policies    []runPolicyEvalItemView
}

type tagBoundPolicyView struct {
	ID    string
	Name  string
	Badge string
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
	mux.HandleFunc("GET /github/logout", u.githubLogout)
	mux.HandleFunc("POST /github/logout", u.githubLogout)
	mux.HandleFunc("GET /profile", u.profile)
	mux.HandleFunc("GET /{$}", u.dashboard)
	mux.HandleFunc("GET /analytics", u.analytics)
	mux.HandleFunc("GET /projects", u.list)
	mux.HandleFunc("GET /projects/new", u.newProject)
	mux.HandleFunc("POST /projects", u.create)
	mux.HandleFunc("POST /folders", u.createFolder)
	mux.HandleFunc("POST /folders/{id}", u.updateFolder)
	mux.HandleFunc("POST /folders/{id}/move", u.moveFolder)
	mux.HandleFunc("POST /folders/{id}/delete", u.deleteFolder)
	mux.HandleFunc("GET /projects/{id}", u.project)
	mux.HandleFunc("GET /projects/{id}/headline", u.projectHeadline)
	mux.HandleFunc("GET /projects/{id}/tabs/tracked", u.tabTracked)
	mux.HandleFunc("GET /projects/{id}/tabs/pulls", u.tabPulls)
	mux.HandleFunc("GET /projects/{id}/tabs/state", u.tabState)
	mux.HandleFunc("GET /projects/{id}/tabs/releases", u.tabReleases)
	mux.HandleFunc("POST /projects/{id}/helm/rollback", u.rollbackHelm)
	mux.HandleFunc("POST /projects/{id}/state/import", u.importState)
	mux.HandleFunc("POST /projects/{id}/state/rollback", u.rollbackState)
	mux.HandleFunc("GET /projects/{id}/tabs/dependencies", u.tabDependencies)
	mux.HandleFunc("POST /projects/{id}/dependencies", u.createDependency)
	mux.HandleFunc("POST /projects/{id}/dependencies/{depID}/trigger-mode", u.updateDependencyTriggerMode)
	mux.HandleFunc("POST /projects/{id}/dependencies/{depID}/delete", u.deleteDependency)
	mux.HandleFunc("POST /projects/{id}/dependencies/{depID}/mappings", u.addDependencyMapping)
	mux.HandleFunc("POST /projects/{id}/dependencies/{depID}/mappings/{mapID}/delete", u.deleteDependencyMapping)
	mux.HandleFunc("GET /projects/{id}/details", u.details)
	mux.HandleFunc("GET /projects/{id}/settings", u.details)
	mux.HandleFunc("POST /projects/{id}", u.update)
	mux.HandleFunc("POST /projects/{id}/policy", u.updateProjectPolicy)
	mux.HandleFunc("POST /projects/{id}/lock", u.lock)
	mux.HandleFunc("POST /projects/{id}/trigger", u.trigger)
	mux.HandleFunc("POST /projects/{id}/drift-check", u.triggerDriftCheck)
	mux.HandleFunc("POST /projects/{id}/extend-ttl", u.extendTTL)
	mux.HandleFunc("POST /projects/{id}/destroy", u.triggerDestroy)
	mux.HandleFunc("POST /projects/{id}/force-unlock", u.forceUnlock)
	mux.HandleFunc("POST /projects/{id}/delete", u.delete)
	mux.HandleFunc("GET /projects/{id}/runs/{runID}", u.runPage)
	mux.HandleFunc("POST /projects/{id}/runs/{runID}/approve", u.approve)
	mux.HandleFunc("POST /projects/{id}/runs/{runID}/unapprove", u.unapprove)
	mux.HandleFunc("POST /projects/{id}/runs/{runID}/confirm", u.confirm)
	mux.HandleFunc("POST /projects/{id}/runs/{runID}/cancel", u.cancel)
	mux.HandleFunc("GET /runs/{id}", u.runPage)
	mux.HandleFunc("GET /runs/{id}/headline", u.runHeadline)
	mux.HandleFunc("GET /runs/{id}/history", u.history)
	mux.HandleFunc("GET /runs/{id}/changes", u.changes)
	mux.HandleFunc("POST /runs/{id}/approve", u.approve)
	mux.HandleFunc("POST /runs/{id}/unapprove", u.unapprove)
	mux.HandleFunc("POST /runs/{id}/confirm", u.confirm)
	mux.HandleFunc("POST /runs/{id}/cancel", u.cancel)
	mux.HandleFunc("GET /contexts", u.contexts)
	mux.HandleFunc("POST /contexts", u.createContext)
	mux.HandleFunc("GET /contexts/{id}", u.contextDetails)
	mux.HandleFunc("POST /contexts/{id}", u.updateContext)
	mux.HandleFunc("POST /contexts/{id}/entries", u.upsertContextEntry)
	mux.HandleFunc("POST /contexts/{id}/entries/{entryID}/delete", u.deleteContextEntry)
	mux.HandleFunc("POST /contexts/{id}/delete", u.deleteContext)
	mux.HandleFunc("GET /cloud-integrations", u.cloudIntegrations)
	mux.HandleFunc("POST /cloud-integrations", u.createCloudIntegration)
	mux.HandleFunc("GET /cloud-integrations/{id}", u.cloudIntegrationDetails)
	mux.HandleFunc("POST /cloud-integrations/{id}", u.updateCloudIntegration)
	mux.HandleFunc("POST /cloud-integrations/{id}/delete", u.deleteCloudIntegration)
	mux.HandleFunc("GET /policies", u.policies)
	mux.HandleFunc("POST /policies", u.createPolicy)
	mux.HandleFunc("POST /policies/test", u.testPolicy)
	mux.HandleFunc("GET /policies/{id}", u.policyDetails)
	mux.HandleFunc("POST /policies/{id}", u.updatePolicy)
	mux.HandleFunc("POST /policies/{id}/delete", u.deletePolicy)
	mux.HandleFunc("GET /audit", u.auditLog)
	mux.HandleFunc("GET /audit/export", u.exportAuditLog)
	mux.Handle("GET /static/", http.FileServer(http.FS(content)))
}

func (u *ui) recordAudit(r *http.Request, tenantID, action, resourceType, resourceID, resourceName, summary string, metadata map[string]any) {
	if u.cfg.Audits == nil {
		return
	}
	info := u.userInfo(r)
	actorLogin := firstNonEmpty(firstNonEmpty(info.Login, u.actor(r)), "system")
	ip := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if ip == "" {
		ip = strings.TrimSpace(r.RemoteAddr)
	}
	_, _ = u.cfg.Audits.Record(r.Context(), auditapp.RecordInput{
		TenantID:     tenantID,
		ActorID:      info.ID,
		ActorLogin:   actorLogin,
		ActorRole:    info.Role,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		ResourceName: resourceName,
		Summary:      summary,
		Metadata:     metadata,
		IPAddress:    ip,
	})
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
	ld := layoutData{Title: title, Body: template.HTML(buf.String())}
	if sp, ok := responsePrincipal(w); ok {
		ld.UserLogin = sp.Login
		ld.UserRole = sp.Role
		ld.TenantID = sp.TenantID
		if sp.Login != "" {
			ld.AvatarURL = "https://github.com/" + sp.Login + ".png?size=64"
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, "layout", ld); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (u *ui) fragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type dashboardProjectRow struct {
	ID, Name, Repo, Branch, TerraformVersion string
	Status, StatusClass, StatusCategory      string
	Locked                                   bool
	Tags                                     []string
	Drifted                                  bool
	Ephemeral                                bool
	ExpiresAt                                string
	Destroyed                                bool
	MonthlyCost                              string
}

type dashboardRunRow struct {
	ID, ProjectID, ProjectName string
	Title, Status, StatusClass string
	Branch, SHA, TriggeredBy   string
	Delta                      template.HTML
	Date                       string
	sortAt                     time.Time
}

type dashboardAttentionItem struct {
	Kind                   string
	BadgeLabel, BadgeClass string
	ProjectID, ProjectName string
	RunID, Title, Branch   string
	Delta                  template.HTML
	Date                   string
	ActionURL, ActionLabel string
	sortAt                 time.Time
}

type dashboardCountItem struct {
	Label string
	Value string
	Count int
}

func (u *ui) dashboard(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if tenantID == "" {
		u.picker(w, r)
		return
	}
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("name")) != "" || strings.TrimSpace(q.Get("branch")) != "" || len(q["tag"]) > 0 {
		u.list(w, r)
		return
	}

	items, err := u.effectiveList(r.Context(), tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	now := u.now()
	cutoff24h := now.Add(-24 * time.Hour)

	projRows := make([]dashboardProjectRow, 0, len(items))
	var recentRuns []dashboardRunRow
	var attention []dashboardAttentionItem

	var (
		countNeedsApproval int
		countFailed        int
		countInProgress    int
		countFinished      int
		countIdle          int
		countLocked        int
		countDrifted       int
		countEphemeral     int
		policyProtected    int
		autoApproveCount   int
		totalRuns          int
		activeRuns         int
		terminalRuns       int
		successfulRuns     int
		runsLast24h        int
		totalMonthlyCost   float64
	)

	tagCounts := make(map[string]int)
	versionCounts := make(map[string]int)

	allRuns, err := u.cfg.Runs.ListSummariesByTenant(r.Context(), tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	runsByProject := make(map[run.ProjectID][]*run.Run, len(items))
	for _, rn := range allRuns {
		pid := rn.Snapshot().ProjectID
		runsByProject[pid] = append(runsByProject[pid], rn)
	}

	for _, p := range items {
		ps := p.Snapshot()
		tags := project.TagList(ps.EffectiveTags)
		for _, t := range tags {
			tagCounts[t]++
		}
		tfVer := ps.TerraformVersion
		if tfVer == "" {
			tfVer = project.DefaultTerraformVersion
		}
		versionCounts[tfVer]++

		if ps.Locked {
			countLocked++
		}
		if len(ps.EffectivePolicies) > 0 {
			policyProtected++
		}
		if ps.AutoApprove {
			autoApproveCount++
		}
		if ps.DriftStatus == project.DriftStatusDrifted {
			countDrifted++
		}
		if ps.TTLHours > 0 || ps.ExpiresAt != nil {
			countEphemeral++
		}

		pRow := dashboardProjectRow{
			ID:               string(ps.ID),
			Name:             ps.Name,
			Repo:             ps.Repo.String(),
			Branch:           ps.DefaultBranch,
			TerraformVersion: tfVer,
			Locked:           ps.Locked,
			Tags:             tags,
			Drifted:          ps.DriftStatus == project.DriftStatusDrifted,
			Ephemeral:        ps.TTLHours > 0 || ps.ExpiresAt != nil,
			ExpiresAt:        fmtTimePtr(ps.ExpiresAt),
		}

		runs := runsByProject[run.ProjectID(ps.ID)]

		for _, rn := range runs {
			rs := rn.Snapshot()
			totalRuns++
			if !rs.CreatedAt.IsZero() && rs.CreatedAt.After(cutoff24h) {
				runsLast24h++
			}
			if !rs.Status.Terminal() {
				activeRuns++
			} else {
				terminalRuns++
				if rs.Status == run.StatusSucceeded || rs.Status == run.StatusPlanned {
					successfulRuns++
				}
			}

			triggered := rs.TriggeredBy
			if triggered == "" {
				triggered = string(rs.Trigger)
			}
			recentRuns = append(recentRuns, dashboardRunRow{
				ID:          string(rs.ID),
				ProjectID:   string(ps.ID),
				ProjectName: ps.Name,
				Title:       firstNonEmpty(runTitle(rs), string(rs.ID)),
				Status:      rs.Status.Label(),
				StatusClass: statusClass(rs.Status),
				Branch:      firstNonEmpty(rs.Branch, ps.DefaultBranch),
				SHA:         shortSHA(rs.SHA),
				TriggeredBy: triggered,
				Delta:       template.HTML(delta(rs)),
				Date:        fmtTime(rs.CreatedAt),
				sortAt:      rs.CreatedAt,
			})

			if rs.Status == run.StatusNeedsApproval {
				attention = append(attention, dashboardAttentionItem{
					Kind:        "approval",
					BadgeLabel:  rs.Status.Label(),
					BadgeClass:  statusClass(rs.Status),
					ProjectID:   string(ps.ID),
					ProjectName: ps.Name,
					RunID:       string(rs.ID),
					Title:       firstNonEmpty(runTitle(rs), "Run awaiting confirmation"),
					Branch:      firstNonEmpty(rs.Branch, ps.DefaultBranch),
					Delta:       template.HTML(delta(rs)),
					Date:        fmtTime(rs.CreatedAt),
					ActionURL:   "/runs/" + string(rs.ID),
					ActionLabel: "Review & confirm",
					sortAt:      rs.CreatedAt,
				})
			}
		}

		if latest := latestSnapshot(runs); latest != nil {
			if latest.MonthlyCost > 0 {
				totalMonthlyCost += latest.MonthlyCost
				pRow.MonthlyCost = run.FormatMonthlyCost(latest.MonthlyCost)
			}
			pRow.Status = latest.Status.Label()
			pRow.StatusClass = statusClass(latest.Status)
			pRow.StatusCategory = statusCategory(latest.Status)
			switch pRow.StatusCategory {
			case "needs_approval":
				countNeedsApproval++
			case "failed":
				countFailed++
				attention = append(attention, dashboardAttentionItem{
					Kind:        "failed",
					BadgeLabel:  "failed",
					BadgeClass:  "pill-failed",
					ProjectID:   string(ps.ID),
					ProjectName: ps.Name,
					RunID:       string(latest.ID),
					Title:       firstNonEmpty(runTitle(*latest), "Latest run failed"),
					Branch:      firstNonEmpty(latest.Branch, ps.DefaultBranch),
					Delta:       template.HTML(delta(*latest)),
					Date:        fmtTime(latest.CreatedAt),
					ActionURL:   "/runs/" + string(latest.ID),
					ActionLabel: "Inspect failure",
					sortAt:      latest.CreatedAt,
				})
			case "in_progress":
				countInProgress++
			case "finished":
				countFinished++
			default:
				countIdle++
			}
		} else {
			countIdle++
		}

		if ps.DriftStatus == project.DriftStatusDrifted {
			driftAt := ps.UpdatedAt
			if ps.LastDriftCheckAt != nil && !ps.LastDriftCheckAt.IsZero() {
				driftAt = *ps.LastDriftCheckAt
			}
			attention = append(attention, dashboardAttentionItem{
				Kind:        "drift",
				BadgeLabel:  "drifted",
				BadgeClass:  "pill-failed",
				ProjectID:   string(ps.ID),
				ProjectName: ps.Name,
				Title:       "Out-of-band cloud state drift detected",
				Branch:      ps.DefaultBranch,
				Date:        fmtTime(driftAt),
				ActionURL:   "/projects/" + string(ps.ID),
				ActionLabel: "Inspect drift",
				sortAt:      driftAt,
			})
		}

		if ps.Locked {
			attention = append(attention, dashboardAttentionItem{
				Kind:        "locked",
				BadgeLabel:  "locked",
				BadgeClass:  "pill-cancelled",
				ProjectID:   string(ps.ID),
				ProjectName: ps.Name,
				Title:       "Workspace is locked against new runs",
				Branch:      ps.DefaultBranch,
				ActionURL:   "/projects/" + string(ps.ID),
				ActionLabel: "Manage lock",
				sortAt:      ps.UpdatedAt,
			})
		}

		projRows = append(projRows, pRow)
	}

	sort.Slice(recentRuns, func(i, j int) bool {
		return recentRuns[i].sortAt.After(recentRuns[j].sortAt)
	})
	if len(recentRuns) > 10 {
		recentRuns = recentRuns[:10]
	}

	sort.Slice(attention, func(i, j int) bool {
		if attention[i].Kind != attention[j].Kind {
			rank := func(k string) int {
				switch k {
				case "approval":
					return 0
				case "failed":
					return 1
				default:
					return 2
				}
			}
			return rank(attention[i].Kind) < rank(attention[j].Kind)
		}
		return attention[i].sortAt.After(attention[j].sortAt)
	})

	successRate := 100
	if terminalRuns > 0 {
		successRate = (successfulRuns * 100) / terminalRuns
	}

	var totalContexts, totalPolicies, totalCloudIntegrations int
	if u.cfg.Contexts != nil {
		if ctxList, err := u.cfg.Contexts.List(r.Context(), tenantID); err == nil {
			totalContexts = len(ctxList)
		}
	}
	if u.cfg.Policies != nil {
		if polList, err := u.cfg.Policies.List(r.Context(), tenantID); err == nil {
			totalPolicies = len(polList)
		}
	}
	if u.cfg.CloudIntegrations != nil {
		if ciList, err := u.cfg.CloudIntegrations.List(r.Context(), tenantID); err == nil {
			totalCloudIntegrations = len(ciList)
		}
	}

	topTags := make([]dashboardCountItem, 0, len(tagCounts))
	for k, c := range tagCounts {
		topTags = append(topTags, dashboardCountItem{Label: k, Value: k, Count: c})
	}
	sort.Slice(topTags, func(i, j int) bool {
		if topTags[i].Count != topTags[j].Count {
			return topTags[i].Count > topTags[j].Count
		}
		return topTags[i].Label < topTags[j].Label
	})
	if len(topTags) > 12 {
		topTags = topTags[:12]
	}

	tfVersions := make([]dashboardCountItem, 0, len(versionCounts))
	for v, c := range versionCounts {
		tfVersions = append(tfVersions, dashboardCountItem{Label: v, Value: v, Count: c})
	}
	sort.Slice(tfVersions, func(i, j int) bool {
		if tfVersions[i].Count != tfVersions[j].Count {
			return tfVersions[i].Count > tfVersions[j].Count
		}
		return tfVersions[i].Label > tfVersions[j].Label
	})

	totalProj := len(items)
	pct := func(n int) int {
		if totalProj == 0 || n == 0 {
			return 0
		}
		p := (n * 100) / totalProj
		if p == 0 {
			return 1
		}
		return p
	}

	u.page(w, "Dashboard", "dashboard", map[string]any{
		"TotalProjects":           totalProj,
		"CountNeedsApproval":      countNeedsApproval,
		"CountFailed":             countFailed,
		"CountInProgress":         countInProgress,
		"CountFinished":           countFinished,
		"CountIdle":               countIdle,
		"CountLocked":             countLocked,
		"CountDrifted":            countDrifted,
		"CountEphemeral":          countEphemeral,
		"TotalMonthlyCost":        run.FormatMonthlyCost(totalMonthlyCost),
		"TotalCloudIntegrations":  totalCloudIntegrations,
		"PctFinished":             pct(countFinished),
		"PctNeedsApproval":        pct(countNeedsApproval),
		"PctInProgress":           pct(countInProgress),
		"PctFailed":               pct(countFailed),
		"PctIdle":                 pct(countIdle),
		"TotalRuns":               totalRuns,
		"ActiveRuns":              activeRuns,
		"SuccessRate":             successRate,
		"RunsLast24h":             runsLast24h,
		"AttentionCount":          len(attention),
		"Attention":               attention,
		"RecentRuns":              recentRuns,
		"Projects":                projRows,
		"TotalContexts":           totalContexts,
		"TotalPolicies":           totalPolicies,
		"PolicyProtectedProjects": policyProtected,
		"AutoApproveProjects":     autoApproveCount,
		"TopTags":                 topTags,
		"TerraformVersions":       tfVersions,
	})
}

type doraChartBucket struct {
	Label   string
	Value   float64
	Display string
}

type doraMetricCard struct {
	Key           string
	Title         string
	Tooltip       string
	HasData       bool
	Value         string
	Subtext       string
	TierLabel     string
	TierClass     string
	EmptyTitle    string
	EmptySubtitle string
	BarSVG        template.HTML
	LineSVG       template.HTML
}

type analyticsScorecardRow struct {
	Name             string
	Link             string
	Workspaces       int
	Deployments      int
	FailedTracked    int
	CFR              string
	CFRClass         string
	MeanLeadTime     string
	MeanApprovalWait string
	Add              int
	Change           int
	Destroy          int
}

type analyticsRiskItem struct {
	Severity    string
	BadgeClass  string
	ProjectID   string
	ProjectName string
	Reason      string
	ActionURL   string
}

type analyticsStaleWorkspace struct {
	ProjectID        string
	ProjectName      string
	DaysSinceLastRun string
	Locked           bool
	TerraformVersion string
	HasPolicy        bool
}

type analyticsHighChurnRun struct {
	RunID       string
	ProjectID   string
	ProjectName string
	Title       string
	Status      string
	StatusClass string
	Delta       template.HTML
	TotalChurn  int
	Date        string
}

func fmtDurationShort(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d >= 48*time.Hour {
		days := int(d / (24 * time.Hour))
		h := int((d % (24 * time.Hour)) / time.Hour)
		return fmt.Sprintf("%dd %dh", days, h)
	}
	if d >= time.Hour {
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		return fmt.Sprintf("%dh %dm", h, m)
	}
	if d >= time.Minute {
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	s := int(d / time.Second)
	if s <= 0 {
		return "<1s"
	}
	return fmt.Sprintf("%ds", s)
}

func renderDORACharts(buckets []doraChartBucket, unitSuffix string) (template.HTML, template.HTML) {
	if len(buckets) == 0 {
		return "", ""
	}
	const (
		width  = 520.0
		height = 176.0
		padL   = 28.0
		padR   = 18.0
		padT   = 22.0
		padB   = 28.0
	)
	plotW := width - padL - padR
	plotH := height - padT - padB

	maxVal := 0.0
	for _, b := range buckets {
		if b.Value > maxVal {
			maxVal = b.Value
		}
	}
	if maxVal <= 0 {
		maxVal = 1.0
	}

	var barBuf strings.Builder
	var lineBuf strings.Builder

	header := fmt.Sprintf(`<svg class="dora-chart-svg" viewBox="0 0 %.0f %.0f" role="img" aria-label="DORA trend chart">`, width, height)
	barBuf.WriteString(header)
	lineBuf.WriteString(header)

	for i := 0; i <= 3; i++ {
		y := padT + (plotH*float64(i))/3.0
		gridLine := fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#eef0f4" stroke-width="1"/>`, padL, y, width-padR, y)
		barBuf.WriteString(gridLine)
		lineBuf.WriteString(gridLine)
	}

	n := len(buckets)
	slotW := plotW / float64(n)
	barW := slotW * 0.56
	if barW > 36 {
		barW = 36
	}
	if barW < 8 {
		barW = 8
	}

	var polyPoints []string
	var areaPoints []string

	for i, b := range buckets {
		cx := padL + slotW*(float64(i)+0.5)
		ratio := b.Value / maxVal
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		h := ratio * plotH
		if b.Value > 0 && h < 4 {
			h = 4
		}
		y := padT + plotH - h

		bx := cx - barW/2
		fill := "#7c3aed"
		if b.Value == 0 {
			fill = "#e5e7eb"
			h = 3
			y = padT + plotH - 3
		}
		barBuf.WriteString(fmt.Sprintf(
			`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="4" fill="%s"><title>%s: %s%s</title></rect>`,
			bx, y, barW, h, fill, template.HTMLEscapeString(b.Label), template.HTMLEscapeString(b.Display), unitSuffix,
		))
		if b.Value > 0 && b.Display != "" {
			barBuf.WriteString(fmt.Sprintf(
				`<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="#4b5563">%s</text>`,
				cx, y-5, template.HTMLEscapeString(b.Display),
			))
		}

		ly := padT + plotH - (ratio * plotH)
		polyPoints = append(polyPoints, fmt.Sprintf("%.1f,%.1f", cx, ly))
		if i == 0 {
			areaPoints = append(areaPoints, fmt.Sprintf("%.1f,%.1f", cx, padT+plotH))
		}
		areaPoints = append(areaPoints, fmt.Sprintf("%.1f,%.1f", cx, ly))
		if i == n-1 {
			areaPoints = append(areaPoints, fmt.Sprintf("%.1f,%.1f", cx, padT+plotH))
		}
	}

	lineBuf.WriteString(fmt.Sprintf(`<polygon points="%s" fill="rgba(124, 58, 237, 0.12)"/>`, strings.Join(areaPoints, " ")))
	lineBuf.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="#7c3aed" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"/>`, strings.Join(polyPoints, " ")))

	for i, b := range buckets {
		cx := padL + slotW*(float64(i)+0.5)
		ratio := b.Value / maxVal
		ly := padT + plotH - (ratio * plotH)
		lineBuf.WriteString(fmt.Sprintf(
			`<circle cx="%.1f" cy="%.1f" r="3.5" fill="#7c3aed" stroke="#ffffff" stroke-width="1.5"><title>%s: %s%s</title></circle>`,
			cx, ly, template.HTMLEscapeString(b.Label), template.HTMLEscapeString(b.Display), unitSuffix,
		))
	}

	labelIndices := []int{0, n / 2, n - 1}
	seenIdx := map[int]bool{}
	for _, idx := range labelIndices {
		if idx < 0 || idx >= n || seenIdx[idx] {
			continue
		}
		seenIdx[idx] = true
		cx := padL + slotW*(float64(idx)+0.5)
		lbl := fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="#6b7280">%s</text>`, cx, height-7, template.HTMLEscapeString(buckets[idx].Label))
		barBuf.WriteString(lbl)
		lineBuf.WriteString(lbl)
	}

	barBuf.WriteString(`</svg>`)
	lineBuf.WriteString(`</svg>`)
	return template.HTML(barBuf.String()), template.HTML(lineBuf.String())
}

func (u *ui) analytics(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if tenantID == "" {
		u.picker(w, r)
		return
	}

	items, err := u.effectiveList(r.Context(), tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	q := r.URL.Query()
	tab := strings.ToLower(strings.TrimSpace(q.Get("tab")))
	if tab != "metrics" {
		tab = "dora"
	}
	windowParam := strings.ToLower(strings.TrimSpace(q.Get("window")))
	var windowDur time.Duration
	var bucketCount int
	switch windowParam {
	case "7d":
		windowDur = 7 * 24 * time.Hour
		bucketCount = 7
	case "90d":
		windowDur = 90 * 24 * time.Hour
		bucketCount = 10
	case "all":
		windowParam = "all"
		windowDur = 90 * 24 * time.Hour
		bucketCount = 10
	default:
		windowParam = "30d"
		windowDur = 30 * 24 * time.Hour
		bucketCount = 10
	}

	selectedTag := strings.TrimSpace(q.Get("tag"))
	selectedProject := strings.TrimSpace(q.Get("project"))

	now := u.now()
	cutoff := now.Add(-windowDur)
	if windowParam == "all" {
		cutoff = time.Time{}
	}

	allTagsSet := make(map[string]bool)
	var filteredProjects []*project.Project
	for _, p := range items {
		ps := p.Snapshot()
		for _, t := range project.TagList(ps.EffectiveTags) {
			allTagsSet[t] = true
		}
		if selectedProject != "" && string(ps.ID) != selectedProject {
			continue
		}
		if selectedTag != "" {
			matched := false
			for _, t := range project.TagList(ps.EffectiveTags) {
				if t == selectedTag {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		filteredProjects = append(filteredProjects, p)
	}

	allTags := make([]string, 0, len(allTagsSet))
	for t := range allTagsSet {
		allTags = append(allTags, t)
	}
	sort.Strings(allTags)

	type recoveryEvent struct {
		At       time.Time
		Duration time.Duration
	}
	type leadEvent struct {
		At       time.Time
		Duration time.Duration
	}
	type deployEvent struct {
		At time.Time
	}
	type terminalTrackedEvent struct {
		At     time.Time
		Failed bool
	}

	var (
		leadEvents        []leadEvent
		deployEvents      []deployEvent
		recoveryEvents    []recoveryEvent
		cfrEvents         []terminalTrackedEvent
		resolvedIncidents int
		activeIncidents   int
	)

	var (
		queueTotalDur    time.Duration
		queueSamples     int
		planTotalDur     time.Duration
		planSamples      int
		approvalTotalDur time.Duration
		approvalSamples  int
		applyTotalDur    time.Duration
		applySamples     int
	)

	type tagAgg struct {
		workspaces   int
		deployments  int
		failed       int
		leadSum      time.Duration
		leadCount    int
		approveSum   time.Duration
		approveCount int
		add          int
		change       int
		destroy      int
	}
	tagStats := make(map[string]*tagAgg)
	var workspaceScorecards []analyticsScorecardRow
	var risks []analyticsRiskItem
	var staleWorkspaces []analyticsStaleWorkspace
	var highChurnRuns []analyticsHighChurnRun

	var (
		totalAdd            int
		totalChange         int
		totalDestroy        int
		destructiveRunCount int
		prPlanRuns          int
		trackedRunsInWindow int
		policyGuardedCount  int
		taggedProjectCount  int
		lockedProjectCount  int
		driftedCount        int
		ephemeralCount      int
		totalMonthlyCost    float64
		costDiffInWindow    float64
	)

	triggerCounts := make(map[string]int)
	authorCounts := make(map[string]int)
	approverCounts := make(map[string]int)
	versionCounts := make(map[string]int)

	allRuns, err := u.cfg.Runs.ListSummariesByTenant(r.Context(), tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	runsByProject := make(map[run.ProjectID][]*run.Run, len(filteredProjects))
	for _, rn := range allRuns {
		pid := rn.Snapshot().ProjectID
		runsByProject[pid] = append(runsByProject[pid], rn)
	}

	for _, p := range filteredProjects {
		ps := p.Snapshot()
		tags := project.TagList(ps.EffectiveTags)
		if len(tags) > 0 {
			taggedProjectCount++
		}
		if len(ps.EffectivePolicies) > 0 {
			policyGuardedCount++
		}
		if ps.DriftStatus == project.DriftStatusDrifted {
			driftedCount++
			risks = append(risks, analyticsRiskItem{
				Severity:    "Drift detected",
				BadgeClass:  "pill-failed",
				ProjectID:   string(ps.ID),
				ProjectName: ps.Name,
				Reason:      "Out-of-band cloud state drift detected on workspace",
				ActionURL:   "/projects/" + string(ps.ID),
			})
		}
		if ps.TTLHours > 0 || ps.ExpiresAt != nil {
			ephemeralCount++
		}
		if ps.Locked {
			lockedProjectCount++
		}
		tfVer := ps.TerraformVersion
		if tfVer == "" {
			tfVer = project.DefaultTerraformVersion
		}
		versionCounts[tfVer]++

		for _, t := range tags {
			st := tagStats[t]
			if st == nil {
				st = &tagAgg{}
				tagStats[t] = st
			}
			st.workspaces++
		}

		if ps.AutoApprove && len(ps.EffectivePolicies) == 0 {
			risks = append(risks, analyticsRiskItem{
				Severity:    "High risk",
				BadgeClass:  "pill-failed",
				ProjectID:   string(ps.ID),
				ProjectName: ps.Name,
				Reason:      "Auto-approve is enabled without an OPA approval policy",
				ActionURL:   "/projects/" + string(ps.ID) + "/settings",
			})
		}
		if len(tags) == 0 {
			risks = append(risks, analyticsRiskItem{
				Severity:    "Governance",
				BadgeClass:  "pill-unconfirmed",
				ProjectID:   string(ps.ID),
				ProjectName: ps.Name,
				Reason:      "Workspace has no ownership or environment tags assigned",
				ActionURL:   "/projects/" + string(ps.ID) + "/settings",
			})
		}

		runs := runsByProject[run.ProjectID(ps.ID)]

		snaps := make([]run.Snapshot, 0, len(runs))
		for _, rn := range runs {
			snaps = append(snaps, rn.Snapshot())
		}
		sort.Slice(snaps, func(i, j int) bool {
			return snaps[i].CreatedAt.Before(snaps[j].CreatedAt)
		})

		var activeFailureAt *time.Time
		var lastRunAt time.Time
		var wsDeployments, wsFailed, wsAdd, wsChange, wsDestroy int
		var wsLeadSum, wsApproveSum time.Duration
		var wsLeadCount, wsApproveCount int

		for _, rs := range snaps {
			if rs.CreatedAt.After(lastRunAt) {
				lastRunAt = rs.CreatedAt
			}
			eventAt := rs.CreatedAt
			if rs.FinishedAt != nil && !rs.FinishedAt.IsZero() {
				eventAt = *rs.FinishedAt
			}

			// Track recovery across chronological tracked runs
			if !rs.PlanOnly {
				if rs.Status == run.StatusFailed && activeFailureAt == nil {
					tFail := eventAt
					activeFailureAt = &tFail
				} else if rs.Status == run.StatusSucceeded && activeFailureAt != nil {
					if eventAt.After(*activeFailureAt) {
						dur := eventAt.Sub(*activeFailureAt)
						if cutoff.IsZero() || !eventAt.Before(cutoff) {
							recoveryEvents = append(recoveryEvents, recoveryEvent{
								At:       eventAt,
								Duration: dur,
							})
							resolvedIncidents++
						}
					}
					activeFailureAt = nil
				}
			}

			if !cutoff.IsZero() && eventAt.Before(cutoff) {
				continue
			}

			trig := string(rs.Trigger)
			if trig == "" {
				trig = "manual"
			}
			triggerCounts[trig]++

			actor := firstNonEmpty(rs.Author, rs.TriggeredBy)
			if actor != "" {
				authorCounts[actor]++
			}
			if len(rs.Approvals) > 0 {
				for _, ap := range rs.Approvals {
					if ap.Login != "" {
						approverCounts[ap.Login]++
					}
				}
			} else if rs.ApprovedBy != "" {
				approverCounts[rs.ApprovedBy]++
			}

			if rs.PlanOnly {
				prPlanRuns++
			} else {
				trackedRunsInWindow++
			}

			totalAdd += rs.Add
			totalChange += rs.Change
			totalDestroy += rs.Destroy
			if !rs.PlanOnly {
				costDiffInWindow += rs.DiffMonthlyCost
			}
			wsAdd += rs.Add
			wsChange += rs.Change
			wsDestroy += rs.Destroy
			for _, t := range tags {
				tagStats[t].add += rs.Add
				tagStats[t].change += rs.Change
				tagStats[t].destroy += rs.Destroy
			}
			if rs.Destroy > 0 {
				destructiveRunCount++
			}
			if churn := rs.Add + rs.Change + rs.Destroy; churn > 0 {
				highChurnRuns = append(highChurnRuns, analyticsHighChurnRun{
					RunID:       string(rs.ID),
					ProjectID:   string(ps.ID),
					ProjectName: ps.Name,
					Title:       firstNonEmpty(runTitle(rs), string(rs.ID)),
					Status:      rs.Status.Label(),
					StatusClass: statusClass(rs.Status),
					Delta:       template.HTML(delta(rs)),
					TotalChurn:  churn,
					Date:        fmtTime(rs.CreatedAt),
				})
			}

			// Phase timing breakdown
			if rs.StartedAt != nil && !rs.CreatedAt.IsZero() && !rs.StartedAt.Before(rs.CreatedAt) {
				queueTotalDur += rs.StartedAt.Sub(rs.CreatedAt)
				queueSamples++
			}
			var planEnd *time.Time
			var initPlanDur time.Duration
			var hasInitPlan bool
			for _, st := range rs.Steps {
				if st.StartedAt != nil && st.FinishedAt != nil && !st.FinishedAt.Before(*st.StartedAt) {
					d := st.FinishedAt.Sub(*st.StartedAt)
					switch st.Name {
					case run.StepInit, run.StepPlan:
						initPlanDur += d
						hasInitPlan = true
						if st.Name == run.StepPlan {
							tEnd := *st.FinishedAt
							planEnd = &tEnd
						}
					case run.StepApply:
						applyTotalDur += d
						applySamples++
					}
				}
			}
			if hasInitPlan {
				planTotalDur += initPlanDur
				planSamples++
			}
			if rs.ApprovedAt != nil {
				waitStart := rs.StartedAt
				if planEnd != nil {
					waitStart = planEnd
				}
				if waitStart == nil && !rs.CreatedAt.IsZero() {
					tCreate := rs.CreatedAt
					waitStart = &tCreate
				}
				if waitStart != nil && !rs.ApprovedAt.Before(*waitStart) {
					waitDur := rs.ApprovedAt.Sub(*waitStart)
					approvalTotalDur += waitDur
					approvalSamples++
					wsApproveSum += waitDur
					wsApproveCount++
					for _, t := range tags {
						tagStats[t].approveSum += waitDur
						tagStats[t].approveCount++
					}
				}
			}

			if !rs.PlanOnly {
				switch rs.Status {
				case run.StatusSucceeded:
					deployEvents = append(deployEvents, deployEvent{At: eventAt})
					cfrEvents = append(cfrEvents, terminalTrackedEvent{At: eventAt, Failed: false})
					wsDeployments++
					for _, t := range tags {
						tagStats[t].deployments++
					}
					if rs.FinishedAt != nil {
						leadStart := rs.CreatedAt
						if rs.CommittedAt != nil && !rs.CommittedAt.IsZero() && !rs.FinishedAt.Before(*rs.CommittedAt) {
							leadStart = *rs.CommittedAt
						}
						if !leadStart.IsZero() && !rs.FinishedAt.Before(leadStart) {
							lt := rs.FinishedAt.Sub(leadStart)
							leadEvents = append(leadEvents, leadEvent{At: eventAt, Duration: lt})
							wsLeadSum += lt
							wsLeadCount++
							for _, t := range tags {
								tagStats[t].leadSum += lt
								tagStats[t].leadCount++
							}
						}
					}
				case run.StatusFailed:
					cfrEvents = append(cfrEvents, terminalTrackedEvent{At: eventAt, Failed: true})
					wsFailed++
					for _, t := range tags {
						tagStats[t].failed++
					}
				}
			}
		}

		if activeFailureAt != nil && now.After(*activeFailureAt) {
			dur := now.Sub(*activeFailureAt)
			if cutoff.IsZero() || !now.Before(cutoff) {
				recoveryEvents = append(recoveryEvents, recoveryEvent{
					At:       now,
					Duration: dur,
				})
				activeIncidents++
			}
			risks = append(risks, analyticsRiskItem{
				Severity:    "Active outage",
				BadgeClass:  "pill-failed",
				ProjectID:   string(ps.ID),
				ProjectName: ps.Name,
				Reason:      fmt.Sprintf("Unresolved failed tracked run for %s", fmtDurationShort(dur)),
				ActionURL:   "/projects/" + string(ps.ID),
			})
		}

		// Staleness check (> 14 days without a run)
		staleThreshold := now.Add(-14 * 24 * time.Hour)
		if lastRunAt.IsZero() || lastRunAt.Before(staleThreshold) {
			daysLabel := "Never run"
			if !lastRunAt.IsZero() {
				days := int(now.Sub(lastRunAt) / (24 * time.Hour))
				daysLabel = fmt.Sprintf("%dd ago", days)
			}
			staleWorkspaces = append(staleWorkspaces, analyticsStaleWorkspace{
				ProjectID:        string(ps.ID),
				ProjectName:      ps.Name,
				DaysSinceLastRun: daysLabel,
				Locked:           ps.Locked,
				TerraformVersion: tfVer,
				HasPolicy:        len(ps.EffectivePolicies) > 0,
			})
		}

		if latest := latestSnapshot(runs); latest != nil && latest.MonthlyCost > 0 {
			totalMonthlyCost += latest.MonthlyCost
		}

		wsTerm := wsDeployments + wsFailed
		wsCFR := "—"
		wsCFRClass := "pill-finished"
		if wsTerm > 0 {
			rate := (float64(wsFailed) * 100.0) / float64(wsTerm)
			wsCFR = fmt.Sprintf("%.1f%%", rate)
			if rate > 30 {
				wsCFRClass = "pill-failed"
			} else if rate > 15 {
				wsCFRClass = "pill-unconfirmed"
			}
		}
		wsLead := "—"
		if wsLeadCount > 0 {
			wsLead = fmtDurationShort(wsLeadSum / time.Duration(wsLeadCount))
		}
		wsApprove := "—"
		if wsApproveCount > 0 {
			wsApprove = fmtDurationShort(wsApproveSum / time.Duration(wsApproveCount))
		}
		workspaceScorecards = append(workspaceScorecards, analyticsScorecardRow{
			Name:             ps.Name,
			Link:             "/projects/" + string(ps.ID),
			Workspaces:       1,
			Deployments:      wsDeployments,
			FailedTracked:    wsFailed,
			CFR:              wsCFR,
			CFRClass:         wsCFRClass,
			MeanLeadTime:     wsLead,
			MeanApprovalWait: wsApprove,
			Add:              wsAdd,
			Change:           wsChange,
			Destroy:          wsDestroy,
		})
	}

	sort.Slice(highChurnRuns, func(i, j int) bool {
		return highChurnRuns[i].TotalChurn > highChurnRuns[j].TotalChurn
	})
	if len(highChurnRuns) > 8 {
		highChurnRuns = highChurnRuns[:8]
	}

	// Build time buckets for the 4 DORA charts
	chartStart := now.Add(-windowDur)
	slotDur := windowDur / time.Duration(bucketCount)
	if slotDur <= 0 {
		slotDur = 24 * time.Hour
	}

	bucketIndex := func(t time.Time) int {
		if t.Before(chartStart) {
			return 0
		}
		idx := int(t.Sub(chartStart) / slotDur)
		if idx < 0 {
			return 0
		}
		if idx >= bucketCount {
			return bucketCount - 1
		}
		return idx
	}
	bucketLabel := func(i int) string {
		bt := chartStart.Add(time.Duration(i+1) * slotDur)
		return bt.Format("Jan 2")
	}

	// 1. Lead Time for Changes card
	leadCard := doraMetricCard{
		Key:           "lead_time",
		Title:         "Lead time for changes",
		Tooltip:       "Mean time from run creation to successful Terraform apply completion",
		EmptyTitle:    "No data available",
		EmptySubtitle: "Lead time is calculated from PR merge commits with tracked runs",
	}
	if len(leadEvents) > 0 {
		var sum time.Duration
		bucketSum := make([]time.Duration, bucketCount)
		bucketCnt := make([]int, bucketCount)
		for _, e := range leadEvents {
			sum += e.Duration
			idx := bucketIndex(e.At)
			bucketSum[idx] += e.Duration
			bucketCnt[idx]++
		}
		avgLead := sum / time.Duration(len(leadEvents))
		leadCard.HasData = true
		leadCard.Value = fmtDurationShort(avgLead)
		leadCard.Subtext = fmt.Sprintf("Across %d successful tracked %s", len(leadEvents), pluralize(len(leadEvents), "run", "runs"))
		switch {
		case avgLead < time.Hour:
			leadCard.TierLabel, leadCard.TierClass = "Elite", "pill-finished"
		case avgLead < 24*time.Hour:
			leadCard.TierLabel, leadCard.TierClass = "High", "pill-finished"
		case avgLead < 7*24*time.Hour:
			leadCard.TierLabel, leadCard.TierClass = "Medium", "pill-unconfirmed"
		default:
			leadCard.TierLabel, leadCard.TierClass = "Low", "pill-failed"
		}
		buckets := make([]doraChartBucket, bucketCount)
		for i := 0; i < bucketCount; i++ {
			var mins float64
			var disp string
			if bucketCnt[i] > 0 {
				d := bucketSum[i] / time.Duration(bucketCnt[i])
				mins = d.Minutes()
				disp = fmtDurationShort(d)
			}
			buckets[i] = doraChartBucket{Label: bucketLabel(i), Value: mins, Display: disp}
		}
		leadCard.BarSVG, leadCard.LineSVG = renderDORACharts(buckets, "")
	}

	// 2. Deployment Frequency card
	deployCard := doraMetricCard{
		Key:           "deployment_frequency",
		Title:         "Deployment frequency",
		Tooltip:       "How often tracked runs complete Terraform apply successfully",
		EmptyTitle:    "No deployments yet",
		EmptySubtitle: "How often tracked runs complete successfully",
	}
	if len(deployEvents) > 0 {
		bucketCnt := make([]int, bucketCount)
		for _, e := range deployEvents {
			bucketCnt[bucketIndex(e.At)]++
		}
		days := windowDur.Hours() / 24.0
		if days < 1 {
			days = 1
		}
		perDay := float64(len(deployEvents)) / days
		deployCard.HasData = true
		deployCard.Value = fmt.Sprintf("%d %s", len(deployEvents), pluralize(len(deployEvents), "deployment", "deployments"))
		if perDay >= 1.0 {
			deployCard.Subtext = fmt.Sprintf("%.1f / day average", perDay)
			deployCard.TierLabel, deployCard.TierClass = "Elite", "pill-finished"
		} else if perDay*7.0 >= 1.0 {
			deployCard.Subtext = fmt.Sprintf("%.1f / week average", perDay*7.0)
			deployCard.TierLabel, deployCard.TierClass = "High", "pill-finished"
		} else {
			deployCard.Subtext = fmt.Sprintf("%.1f / month average", perDay*30.0)
			deployCard.TierLabel, deployCard.TierClass = "Medium", "pill-unconfirmed"
		}
		buckets := make([]doraChartBucket, bucketCount)
		for i := 0; i < bucketCount; i++ {
			disp := ""
			if bucketCnt[i] > 0 {
				disp = strconv.Itoa(bucketCnt[i])
			}
			buckets[i] = doraChartBucket{Label: bucketLabel(i), Value: float64(bucketCnt[i]), Display: disp}
		}
		deployCard.BarSVG, deployCard.LineSVG = renderDORACharts(buckets, " deployments")
	}

	// 3. Average Time to Recovery (MTTR) card
	mttrCard := doraMetricCard{
		Key:           "mttr",
		Title:         "Average time to recovery",
		Tooltip:       "Mean elapsed time from a failed tracked run to the next successful tracked run on the same workspace",
		EmptyTitle:    "No recovery data available",
		EmptySubtitle: "Recovery time is measured from stack failure to next successful tracked run",
	}
	if len(recoveryEvents) > 0 {
		var sum time.Duration
		bucketSum := make([]time.Duration, bucketCount)
		bucketCnt := make([]int, bucketCount)
		for _, e := range recoveryEvents {
			sum += e.Duration
			idx := bucketIndex(e.At)
			bucketSum[idx] += e.Duration
			bucketCnt[idx]++
		}
		avgMTTR := sum / time.Duration(len(recoveryEvents))
		mttrCard.HasData = true
		mttrCard.Value = fmtDurationShort(avgMTTR)
		switch {
		case resolvedIncidents > 0 && activeIncidents > 0:
			mttrCard.Subtext = fmt.Sprintf("Across %d resolved, %d active %s", resolvedIncidents, activeIncidents, pluralize(activeIncidents, "incident", "incidents"))
		case activeIncidents > 0:
			mttrCard.Subtext = fmt.Sprintf("Across %d active unresolved %s", activeIncidents, pluralize(activeIncidents, "incident", "incidents"))
		default:
			mttrCard.Subtext = fmt.Sprintf("Across %d resolved %s", resolvedIncidents, pluralize(resolvedIncidents, "incident", "incidents"))
		}
		switch {
		case avgMTTR < time.Hour:
			mttrCard.TierLabel, mttrCard.TierClass = "Elite", "pill-finished"
		case avgMTTR < 24*time.Hour:
			mttrCard.TierLabel, mttrCard.TierClass = "High", "pill-finished"
		case avgMTTR < 7*24*time.Hour:
			mttrCard.TierLabel, mttrCard.TierClass = "Medium", "pill-unconfirmed"
		default:
			mttrCard.TierLabel, mttrCard.TierClass = "Low", "pill-failed"
		}
		buckets := make([]doraChartBucket, bucketCount)
		for i := 0; i < bucketCount; i++ {
			var mins float64
			var disp string
			if bucketCnt[i] > 0 {
				d := bucketSum[i] / time.Duration(bucketCnt[i])
				mins = d.Minutes()
				disp = fmtDurationShort(d)
			}
			buckets[i] = doraChartBucket{Label: bucketLabel(i), Value: mins, Display: disp}
		}
		mttrCard.BarSVG, mttrCard.LineSVG = renderDORACharts(buckets, "")
	}

	// 4. Change Failure Rate (CFR) card
	cfrCard := doraMetricCard{
		Key:           "change_failure_rate",
		Title:         "Change failure rate",
		Tooltip:       "Percentage of completed tracked runs that ended in failure",
		EmptyTitle:    "No failure data available",
		EmptySubtitle: "Percentage of tracked runs that fail",
	}
	if len(cfrEvents) > 0 {
		failedCount := 0
		bucketTotal := make([]int, bucketCount)
		bucketFailed := make([]int, bucketCount)
		for _, e := range cfrEvents {
			idx := bucketIndex(e.At)
			bucketTotal[idx]++
			if e.Failed {
				failedCount++
				bucketFailed[idx]++
			}
		}
		rate := (float64(failedCount) * 100.0) / float64(len(cfrEvents))
		cfrCard.HasData = true
		cfrCard.Value = fmt.Sprintf("%.1f%%", rate)
		cfrCard.Subtext = fmt.Sprintf("%d failed of %d tracked %s", failedCount, len(cfrEvents), pluralize(len(cfrEvents), "run", "runs"))
		switch {
		case rate <= 15.0:
			cfrCard.TierLabel, cfrCard.TierClass = "Elite", "pill-finished"
		case rate <= 30.0:
			cfrCard.TierLabel, cfrCard.TierClass = "High", "pill-finished"
		case rate <= 45.0:
			cfrCard.TierLabel, cfrCard.TierClass = "Medium", "pill-unconfirmed"
		default:
			cfrCard.TierLabel, cfrCard.TierClass = "Low", "pill-failed"
		}
		buckets := make([]doraChartBucket, bucketCount)
		for i := 0; i < bucketCount; i++ {
			var rVal float64
			var disp string
			if bucketTotal[i] > 0 {
				rVal = (float64(bucketFailed[i]) * 100.0) / float64(bucketTotal[i])
				disp = fmt.Sprintf("%.0f%%", rVal)
			}
			buckets[i] = doraChartBucket{Label: bucketLabel(i), Value: rVal, Display: disp}
		}
		cfrCard.BarSVG, cfrCard.LineSVG = renderDORACharts(buckets, "")
	}

	// Pipeline & Approval Bottleneck averages
	var avgQueue, avgPlan, avgApproval, avgApply time.Duration
	if queueSamples > 0 {
		avgQueue = queueTotalDur / time.Duration(queueSamples)
	}
	if planSamples > 0 {
		avgPlan = planTotalDur / time.Duration(planSamples)
	}
	if approvalSamples > 0 {
		avgApproval = approvalTotalDur / time.Duration(approvalSamples)
	}
	if applySamples > 0 {
		avgApply = applyTotalDur / time.Duration(applySamples)
	}
	totalCycleAvg := avgQueue + avgPlan + avgApproval + avgApply
	phasePct := func(d time.Duration) int {
		if totalCycleAvg <= 0 || d <= 0 {
			return 0
		}
		p := int((d * 100) / totalCycleAvg)
		if p == 0 {
			return 1
		}
		return p
	}

	// Team / Tag scorecards
	tagScorecards := make([]analyticsScorecardRow, 0, len(tagStats))
	for tag, st := range tagStats {
		term := st.deployments + st.failed
		cfr := "—"
		cfrClass := "pill-finished"
		if term > 0 {
			rVal := (float64(st.failed) * 100.0) / float64(term)
			cfr = fmt.Sprintf("%.1f%%", rVal)
			if rVal > 30 {
				cfrClass = "pill-failed"
			} else if rVal > 15 {
				cfrClass = "pill-unconfirmed"
			}
		}
		lt := "—"
		if st.leadCount > 0 {
			lt = fmtDurationShort(st.leadSum / time.Duration(st.leadCount))
		}
		apWait := "—"
		if st.approveCount > 0 {
			apWait = fmtDurationShort(st.approveSum / time.Duration(st.approveCount))
		}
		tagScorecards = append(tagScorecards, analyticsScorecardRow{
			Name:             tag,
			Link:             "/analytics?tab=" + tab + "&window=" + windowParam + "&tag=" + tag,
			Workspaces:       st.workspaces,
			Deployments:      st.deployments,
			FailedTracked:    st.failed,
			CFR:              cfr,
			CFRClass:         cfrClass,
			MeanLeadTime:     lt,
			MeanApprovalWait: apWait,
			Add:              st.add,
			Change:           st.change,
			Destroy:          st.destroy,
		})
	}
	sort.Slice(tagScorecards, func(i, j int) bool {
		if tagScorecards[i].Deployments != tagScorecards[j].Deployments {
			return tagScorecards[i].Deployments > tagScorecards[j].Deployments
		}
		return tagScorecards[i].Name < tagScorecards[j].Name
	})

	sort.Slice(workspaceScorecards, func(i, j int) bool {
		if workspaceScorecards[i].Deployments != workspaceScorecards[j].Deployments {
			return workspaceScorecards[i].Deployments > workspaceScorecards[j].Deployments
		}
		return workspaceScorecards[i].Name < workspaceScorecards[j].Name
	})

	mapToSortedCounts := func(m map[string]int) []dashboardCountItem {
		out := make([]dashboardCountItem, 0, len(m))
		for k, c := range m {
			out = append(out, dashboardCountItem{Label: k, Value: k, Count: c})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Count != out[j].Count {
				return out[i].Count > out[j].Count
			}
			return out[i].Label < out[j].Label
		})
		return out
	}

	policyCoveragePct := 0
	tagCoveragePct := 0
	if len(filteredProjects) > 0 {
		policyCoveragePct = (policyGuardedCount * 100) / len(filteredProjects)
		tagCoveragePct = (taggedProjectCount * 100) / len(filteredProjects)
	}

	u.page(w, "Analytics", "analytics", map[string]any{
		"Tab":                 tab,
		"Window":              windowParam,
		"SelectedTag":         selectedTag,
		"SelectedProject":     selectedProject,
		"AllTags":             allTags,
		"TotalWorkspaces":     len(filteredProjects),
		"DORACards":           []doraMetricCard{leadCard, deployCard, mttrCard, cfrCard},
		"AvgQueue":            fmtDurationShort(avgQueue),
		"AvgPlan":             fmtDurationShort(avgPlan),
		"AvgApproval":         fmtDurationShort(avgApproval),
		"AvgApply":            fmtDurationShort(avgApply),
		"QueueSamples":        queueSamples,
		"PlanSamples":         planSamples,
		"ApprovalSamples":     approvalSamples,
		"ApplySamples":        applySamples,
		"PctQueue":            phasePct(avgQueue),
		"PctPlan":             phasePct(avgPlan),
		"PctApproval":         phasePct(avgApproval),
		"PctApply":            phasePct(avgApply),
		"HasPhaseData":        totalCycleAvg > 0,
		"TagScorecards":       tagScorecards,
		"WorkspaceScorecards": workspaceScorecards,
		"TotalAdd":            totalAdd,
		"TotalChange":         totalChange,
		"TotalDestroy":        totalDestroy,
		"DestructiveRunCount": destructiveRunCount,
		"HighChurnRuns":       highChurnRuns,
		"Risks":               risks,
		"StaleWorkspaces":     staleWorkspaces,
		"PolicyCoveragePct":   policyCoveragePct,
		"TagCoveragePct":      tagCoveragePct,
		"LockedProjectCount":  lockedProjectCount,
		"TerraformVersions":   mapToSortedCounts(versionCounts),
		"TriggerBreakdown":    mapToSortedCounts(triggerCounts),
		"TopAuthors":          mapToSortedCounts(authorCounts),
		"TopApprovers":        mapToSortedCounts(approverCounts),
		"PRPlanRuns":          prPlanRuns,
		"TrackedRunsInWindow": trackedRunsInWindow,
		"DriftedWorkspaces":   driftedCount,
		"EphemeralWorkspaces": ephemeralCount,
		"TotalMonthlyCost":    run.FormatMonthlyCost(totalMonthlyCost),
		"CostDiffInWindow":    run.FormatCostDiff(costDiffInWindow),
	})
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
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
	items, err := u.effectiveList(r.Context(), tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tree, err := u.folderTree(r.Context(), tenantID, strings.TrimSpace(r.URL.Query().Get("folder")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filterName := strings.TrimSpace(r.URL.Query().Get("name"))
	filterBranch := strings.TrimSpace(r.URL.Query().Get("branch"))
	filterTag := strings.TrimSpace(strings.Join(r.URL.Query()["tag"], ", "))
	rows := make([]projectRow, 0, len(items))
	var countNeedsApproval, countFailed, countInProgress, countFinished int
	for _, p := range items {
		ps := p.Snapshot()
		tags := project.TagList(ps.EffectiveTags)
		if !matchesProjectFilter(ps.Name, ps.DefaultBranch, tags, filterName, filterBranch, filterTag) {
			continue
		}
		row := projectRow{
			ID: string(ps.ID), Name: ps.Name, Repo: ps.Repo.String(), Branch: ps.DefaultBranch,
			FolderID: ps.FolderID, FolderPath: tree.paths[ps.FolderID],
			Tags: tags, TagsCSV: strings.Join(tags, ","),
			DriftEnabled:     ps.DriftEnabled,
			DriftStatus:      ps.DriftStatus,
			DriftStatusLabel: driftStatusLabel(ps.DriftStatus),
			TTLHours:         ps.TTLHours,
			AutoDestroy:      ps.AutoDestroy,
			ExpiresAt:        fmtTimePtr(ps.ExpiresAt),
		}
		runs, err := u.cfg.Runs.ListByProject(r.Context(), ps.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if latest := latestSnapshot(runs); latest != nil {
			row.Status = latest.Status.Label()
			row.StatusClass = statusClass(latest.Status)
			row.StatusCategory = statusCategory(latest.Status)
			if latest.MonthlyCost > 0 {
				row.EstMonthlyCost = run.FormatMonthlyCost(latest.MonthlyCost)
			} else if len(latest.PlanJSON) > 0 {
				if parsed, err := run.ParsePlanJSON(latest.PlanJSON); err == nil && parsed != nil && parsed.Cost != nil && parsed.Cost.MonthlyCost > 0 {
					row.EstMonthlyCost = run.FormatMonthlyCost(parsed.Cost.MonthlyCost)
				}
			}
			switch row.StatusCategory {
			case "needs_approval":
				countNeedsApproval++
			case "failed":
				countFailed++
			case "in_progress":
				countInProgress++
			case "finished":
				countFinished++
			}
		}
		rows = append(rows, row)
	}
	u.page(w, "Projects", "projects", map[string]any{
		"Projects":           rows,
		"Folders":            tree.nodes,
		"SelectedFolder":     tree.selected,
		"HasFolders":         len(tree.nodes) > 0,
		"NewFolderFields":    folderFields{Policies: u.policyChoicesFor(r.Context(), tenantID, nil)},
		"EditFolderFields":   folderFields{Policies: u.policyChoicesFor(r.Context(), tenantID, tree.selected.PolicyIDs), PassDown: tree.selected.PassDown, TagsSet: tree.selected.TagsSet, Tags: tree.selected.Tags, IsRoot: tree.selected.IsRoot},
		"Policies":           u.policyChoices(r.Context(), tenantID, ""),
		"FilterName":         filterName,
		"FilterBranch":       filterBranch,
		"FilterTag":          filterTag,
		"KnownTags":          collectKnownTags(items),
		"KnownBranches":      collectKnownBranches(items),
		"CountAll":           len(rows),
		"CountNeedsApproval": countNeedsApproval,
		"CountFailed":        countFailed,
		"CountInProgress":    countInProgress,
		"CountFinished":      countFinished,
	})
}

func collectKnownBranches(items []*project.Project) []string {
	seen := make(map[string]struct{})
	for _, p := range items {
		if b := strings.TrimSpace(p.Snapshot().DefaultBranch); b != "" {
			seen[b] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for b := range seen {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

func collectKnownTags(items []*project.Project) []string {
	seen := make(map[string]struct{})
	for _, p := range items {
		for _, t := range project.TagList(p.Snapshot().EffectiveTags) {
			seen[t] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func (u *ui) knownTags(ctx context.Context, tenantID string) []string {
	if tenantID == "" {
		return nil
	}
	seen := make(map[string]struct{})
	if u.cfg.Projects != nil {
		if items, err := u.effectiveList(ctx, tenantID); err == nil {
			for _, p := range items {
				for _, t := range project.TagList(p.Snapshot().EffectiveTags) {
					seen[t] = struct{}{}
				}
			}
		}
	}
	if u.cfg.Contexts != nil {
		if items, err := u.cfg.Contexts.List(ctx, tenantID); err == nil {
			for _, c := range items {
				for _, t := range project.TagList(c.Snapshot().Tags) {
					seen[t] = struct{}{}
				}
			}
		}
	}
	if u.cfg.Policies != nil {
		if items, err := u.cfg.Policies.List(ctx, tenantID); err == nil {
			for _, p := range items {
				for _, t := range project.TagList(p.Snapshot().Tags) {
					seen[t] = struct{}{}
				}
			}
		}
	}
	if u.cfg.CloudIntegrations != nil {
		if items, err := u.cfg.CloudIntegrations.List(ctx, tenantID); err == nil {
			for _, ci := range items {
				for _, t := range project.TagList(ci.Snapshot().Tags) {
					seen[t] = struct{}{}
				}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func matchesProjectFilter(name, branch string, tags []string, filterName, filterBranch, filterTag string) bool {
	if filterName != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(filterName)) {
		return false
	}
	if filterBranch != "" && !strings.Contains(strings.ToLower(branch), strings.ToLower(filterBranch)) {
		return false
	}
	if filterTag != "" {
		needles := strings.Fields(strings.ReplaceAll(strings.ToLower(filterTag), ",", " "))
		for _, needle := range needles {
			matched := false
			for _, t := range tags {
				if strings.Contains(strings.ToLower(t), needle) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
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
	tree, err := u.folderTree(r.Context(), tenantID, strings.TrimSpace(r.URL.Query().Get("folder")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	defaultBranch := "main"
	if len(repos) > 0 && strings.TrimSpace(repos[0].DefaultBranch) != "" {
		defaultBranch = strings.TrimSpace(repos[0].DefaultBranch)
	}
	u.page(w, "New project", "new_project", map[string]any{
		"Repos":             repos,
		"DefaultBranch":     defaultBranch,
		"KnownTags":         u.knownTags(r.Context(), tenantID),
		"Versions":          u.versions(r.Context()),
		"DefaultVersion":    project.DefaultTerraformVersion,
		"Contexts":          u.contextChoices(r.Context(), tenantID, ""),
		"Policies":          u.policyChoices(r.Context(), tenantID, ""),
		"CloudIntegrations": u.cloudIntegrationChoices(r.Context(), tenantID, ""),
		"EngineForm":        engineFormFrom(nil),
		"Folders":           tree.nodes,
		"FolderID":          tree.selected.ID,
		"FolderPolicies":    u.applicableFolderPolicies(r.Context(), tenantID, tree.selected.ID),
		"InheritedTags":     project.FormatTags(u.folderEffectiveTags(r.Context(), tenantID, tree.selected.ID)),
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
	replacePolicies := r.FormValue("policy_field") == "1"
	var policyIDs []string
	if replacePolicies {
		policyIDs = r.Form["policy_id"]
		if !u.validPolicyIDs(r, tenantID, policyIDs) {
			http.Error(w, "invalid policy_id", http.StatusBadRequest)
			return
		}
	}
	folderID := strings.TrimSpace(r.FormValue("folder_id"))
	if folderID != "" && u.cfg.Folders != nil {
		if _, err := u.cfg.Folders.Get(r.Context(), tenantID, folderID); err != nil {
			u.folderError(w, err)
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
	var tags map[string]string
	overrideTags := r.FormValue("tags_override") == "on"
	if overrideTags {
		tags, err = project.ParseTags(r.FormValue("tags"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	sourceRepo := project.SourceRepo{Owner: match.Owner, Name: match.Name}
	branch := strings.TrimSpace(r.FormValue("branch"))
	if branch == "" && strings.TrimSpace(match.DefaultBranch) != "" {
		branch = strings.TrimSpace(match.DefaultBranch)
	}
	p, err := u.cfg.Projects.Create(r.Context(), tenantID, r.FormValue("name"), sourceRepo, match.ID, branch, r.FormValue("workdir"), r.FormValue("auto_approve") == "on")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if folderID != "" && u.cfg.Folders != nil {
		if err := u.cfg.Projects.MoveToFolder(r.Context(), p.Snapshot().ID, folderID, policyIDs, replacePolicies); err != nil {
			u.folderError(w, err)
			return
		}
	} else if replacePolicies {
		if err := u.editProject(r.Context(), p.Snapshot().ID, func(p *project.Project) error {
			p.SetPolicyIDs(policyIDs)
			return nil
		}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if overrideTags {
		if err := u.editProject(r.Context(), p.Snapshot().ID, func(p *project.Project) error {
			return p.SetTagOverride(tags)
		}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := u.cfg.Projects.SetTerraformVersion(r.Context(), p.Snapshot().ID, r.FormValue("terraform_version")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := u.cfg.Projects.SetEngine(r.Context(), p.Snapshot().ID, r.FormValue("engine"), engineConfigFromRequest(r)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.FormValue("drift_enabled") == "on" {
		interval, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("drift_interval_minutes")))
		if err := u.cfg.Projects.SetDriftConfig(r.Context(), p.Snapshot().ID, true, interval); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if ttlHours, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("ttl_hours"))); ttlHours > 0 || r.FormValue("auto_destroy") == "on" {
		if err := u.cfg.Projects.SetTTL(r.Context(), p.Snapshot().ID, ttlHours, r.FormValue("auto_destroy") == "on"); err != nil {
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
	if u.cfg.CloudIntegrations != nil {
		if err := u.cfg.CloudIntegrations.SetProjectIntegrations(r.Context(), tenantID, p.Snapshot().ID, parseCloudIntegrationIDs(r.Form["cloud_integration_ids"])); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	u.recordAudit(r, tenantID, "workspace.created", "workspace", string(p.Snapshot().ID), p.Snapshot().Name,
		fmt.Sprintf("Created workspace %s (%s)", p.Snapshot().Name, sourceRepo.String()),
		map[string]any{"repo": sourceRepo.String(), "branch": branch, "tags": project.FormatTags(tags), "folder_id": folderID, "policy_ids": policyIDs})
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
	ps := p.Snapshot()
	meta, err := u.cfg.States.Metadata(r.Context(), string(ps.ID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	versions, err := u.cfg.States.ListVersions(r.Context(), string(ps.ID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var diff *state.StateDiff
	fromQ, toQ := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if fromQ != "" || toQ != "" {
		fromSerial, err1 := strconv.ParseInt(fromQ, 10, 64)
		toSerial, err2 := strconv.ParseInt(toQ, 10, 64)
		if err1 != nil || err2 != nil || fromSerial == toSerial {
			http.Error(w, "choose two different state serials", http.StatusBadRequest)
			return
		}
		d, err := u.cfg.States.Diff(r.Context(), string(ps.ID), fromSerial, toSerial)
		if err != nil {
			if errors.Is(err, state.ErrVersionNotFound) {
				http.Error(w, "state version not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		diff = &d
	}
	u.fragment(w, "state_history", stateHistoryTab(ps.ID, meta, versions, diff, anyNonTerminal(runs)))
}

func (u *ui) importState(w http.ResponseWriter, r *http.Request) {
	p, runs, err := u.loadProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	if ps.Locked || anyNonTerminal(runs) {
		http.Error(w, "cannot import state while project is locked or a run is active", http.StatusConflict)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("state_file")
	if err != nil {
		http.Error(w, "state_file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 32<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := u.actor(r)
	if actor == "" {
		actor = "import"
	}
	if err := u.cfg.States.ImportWithActor(r.Context(), string(ps.ID), data, actor); err != nil {
		if errors.Is(err, state.ErrLocked) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, state.ErrInvalidState) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.recordAudit(r, ps.TenantID, "state.imported", "state", string(ps.ID), ps.Name,
		fmt.Sprintf("Imported Terraform state (%d B) into workspace %s", len(data), ps.Name),
		map[string]any{"size_bytes": len(data)})
	if r.Header.Get("HX-Request") == "true" {
		u.tabState(w, r)
		return
	}
	http.Redirect(w, r, "/projects/"+string(ps.ID)+"/tabs/state", http.StatusSeeOther)
}

func (u *ui) rollbackState(w http.ResponseWriter, r *http.Request) {
	p, runs, err := u.loadProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	if ps.Locked || anyNonTerminal(runs) {
		http.Error(w, "cannot restore state while project is locked or a run is active", http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	serial, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("serial")), 10, 64)
	if err != nil {
		http.Error(w, "invalid serial", http.StatusBadRequest)
		return
	}
	actor := u.actor(r)
	if actor == "" {
		actor = "rollback"
	}
	newSerial, err := u.cfg.States.Rollback(r.Context(), string(ps.ID), serial, actor)
	if err != nil {
		if errors.Is(err, state.ErrVersionNotFound) {
			http.Error(w, "state version not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, state.ErrLocked) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, state.ErrInvalidState) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.recordAudit(r, ps.TenantID, "state.restored", "state", string(ps.ID), ps.Name,
		fmt.Sprintf("Restored state serial %d as serial %d on workspace %s", serial, newSerial, ps.Name),
		map[string]any{"from_serial": serial, "serial": newSerial})
	if r.Header.Get("HX-Request") == "true" {
		u.tabState(w, r)
		return
	}
	http.Redirect(w, r, "/projects/"+string(ps.ID)+"/tabs/state", http.StatusSeeOther)
}

type helmRevisionRow struct {
	Revision    int
	Updated     string
	Status      string
	Chart       string
	AppVersion  string
	Description string
	Current     bool
}

func (u *ui) tabReleases(w http.ResponseWriter, r *http.Request) {
	p, runs, err := u.loadProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	view := map[string]any{
		"ProjectID": string(ps.ID),
		"IsHelm":    ps.Engine == project.EngineHelm,
		"Release":   ps.EngineConfig.ReleaseName,
		"Namespace": ps.EngineConfig.HelmNamespace,
		"Busy":      ps.Locked || anyNonTerminal(runs),
		"Poll":      anyNonTerminal(runs),
	}
	if ps.Engine == project.EngineHelm && u.cfg.HelmReleases != nil {
		release, namespace, revisions, err := u.cfg.HelmReleases.List(r.Context(), string(ps.ID))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if release != "" {
			view["Release"] = release
		}
		if namespace != "" {
			view["Namespace"] = namespace
		}
		rows := make([]helmRevisionRow, 0, len(revisions))
		for i, rev := range revisions {
			rows = append(rows, helmRevisionRow{
				Revision: rev.Revision, Updated: rev.Updated, Status: rev.Status,
				Chart: rev.Chart, AppVersion: rev.AppVersion, Description: rev.Description,
				Current: i == 0,
			})
		}
		view["Revisions"] = rows
	}
	u.fragment(w, "helm_releases", view)
}

func (u *ui) rollbackHelm(w http.ResponseWriter, r *http.Request) {
	p, runs, err := u.loadProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	if ps.Engine != project.EngineHelm {
		http.Error(w, "workspace engine is not Helm", http.StatusBadRequest)
		return
	}
	if ps.Locked || anyNonTerminal(runs) {
		http.Error(w, "cannot roll back a Helm release while the project is locked or a run is active", http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	revision, err := strconv.Atoi(strings.TrimSpace(r.FormValue("revision")))
	if err != nil || revision <= 0 {
		http.Error(w, "invalid revision", http.StatusBadRequest)
		return
	}
	if u.cfg.HelmReleases == nil {
		http.Error(w, "Helm release history is not configured", http.StatusInternalServerError)
		return
	}
	_, _, revisions, err := u.cfg.HelmReleases.List(r.Context(), string(ps.ID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	found := false
	for _, rev := range revisions {
		if rev.Revision == revision {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "revision is not in the stored Helm history", http.StatusBadRequest)
		return
	}
	if u.cfg.Tokens == nil || u.cfg.Checkout == nil {
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
	release := ps.EngineConfig.ReleaseName
	auto := true
	opened, err := u.cfg.Runs.Open(r.Context(), runapp.OpenRequest{
		ProjectID: ps.ID, Trigger: run.TriggerManual, SHA: sha, Branch: ps.DefaultBranch,
		Message: fmt.Sprintf("Rollback Helm release %s to revision %d", release, revision),
		Author:  u.actor(r), TriggeredBy: u.actor(r),
		AutoApprove: &auto, HelmRollbackRevision: revision,
	})
	if err != nil {
		if errors.Is(err, runapp.ErrDuplicate) {
			http.Redirect(w, r, "/projects/"+string(ps.ID), http.StatusSeeOther)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rs := opened.Snapshot()
	u.recordAudit(r, ps.TenantID, "helm.rollback", "run", string(rs.ID), ps.Name,
		fmt.Sprintf("Queued Helm rollback of %s to revision %d on workspace %s", release, revision, ps.Name),
		map[string]any{"project_id": string(ps.ID), "release": release, "revision": revision})
	http.Redirect(w, r, "/runs/"+string(rs.ID), http.StatusSeeOther)
}

func (u *ui) details(w http.ResponseWriter, r *http.Request) {
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	driftInterval := ps.DriftIntervalMinutes
	if driftInterval <= 0 {
		driftInterval = project.DefaultDriftIntervalMinutes
	}
	tree, err := u.folderTree(r.Context(), ps.TenantID, ps.FolderID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ownPolicies := u.policyChoicesFor(r.Context(), ps.TenantID, ps.PolicyIDs)
	tagText := project.FormatTags(ps.EffectiveTags)
	tagsLabel := "Inherited"
	if ps.TagsOverridden {
		tagsLabel = "Override"
	}
	u.page(w, ps.Name+" details", "details", map[string]any{
		"Breadcrumbs":     tree.crumbs(ps.FolderID),
		"Folders":         tree.nodes,
		"FolderID":        ps.FolderID,
		"AppliedPolicies": u.appliedFolderPolicies(r.Context(), ps),
		"TagsOverridden":  ps.TagsOverridden,
		"TagsLabel":       tagsLabel,
		"ID":              string(ps.ID), "Name": ps.Name, "Repo": ps.Repo.String(),
		"AutoApprove": ps.AutoApprove, "Locked": ps.Locked,
		"Branch": ps.DefaultBranch, "Workdir": ps.WorkingDirectory,
		"TerraformVersion":          ps.TerraformVersion,
		"DriftEnabled":              ps.DriftEnabled,
		"DriftIntervalMinutes":      driftInterval,
		"DriftStatus":               ps.DriftStatus,
		"DriftStatusLabel":          driftStatusLabel(ps.DriftStatus),
		"LastDriftCheckAt":          fmtTimePtr(ps.LastDriftCheckAt),
		"TTLHours":                  ps.TTLHours,
		"AutoDestroy":               ps.AutoDestroy,
		"ExpiresAt":                 fmtTimePtr(ps.ExpiresAt),
		"Tags":                      tagText,
		"TagList":                   project.TagList(ps.EffectiveTags),
		"KnownTags":                 u.knownTags(r.Context(), ps.TenantID),
		"Versions":                  u.versions(r.Context()),
		"Contexts":                  u.contextChoices(r.Context(), ps.TenantID, ps.ID),
		"Policies":                  ownPolicies,
		"TagBoundPolicies":          u.tagBoundPolicies(r.Context(), ps.TenantID, effectivePolicyIDs(ps), ps.EffectiveTags),
		"CloudIntegrations":         u.cloudIntegrationChoices(r.Context(), ps.TenantID, ps.ID),
		"TagBoundCloudIntegrations": u.tagBoundCloudIntegrations(r.Context(), ps.TenantID, ps.ID, ps.EffectiveTags),
		"EngineForm":                engineFormFrom(&ps),
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
	newFolderID := strings.TrimSpace(r.FormValue("folder_id"))
	moved := u.cfg.Folders != nil && newFolderID != "" && newFolderID != ps.FolderID
	overrideTags := r.FormValue("tags_override") == "on"
	var tags map[string]string
	if overrideTags && !moved {
		tags, err = project.ParseTags(r.FormValue("tags"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	replacePolicies := r.FormValue("policy_field") == "1"
	var policyIDs []string
	if replacePolicies {
		policyIDs = r.Form["policy_id"]
		if !u.validPolicyIDs(r, ps.TenantID, policyIDs) {
			http.Error(w, "invalid policy_id", http.StatusBadRequest)
			return
		}
	}
	if moved {
		if err := u.cfg.Projects.MoveToFolder(r.Context(), id, newFolderID, policyIDs, replacePolicies); err != nil {
			u.folderError(w, err)
			return
		}
	}
	driftEnabled := r.FormValue("drift_enabled") == "on"
	driftInterval, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("drift_interval_minutes")))
	ttlHours, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("ttl_hours")))
	autoDestroy := r.FormValue("auto_destroy") == "on"
	updateEngine := r.Form.Has("engine")
	if _, err := u.cfg.Projects.Update(r.Context(), projectapp.UpdateInput{
		ID: id, Name: ps.Name, Branch: r.FormValue("branch"), Workdir: r.FormValue("workdir"),
		UpdateEngine:         updateEngine,
		Engine:               r.FormValue("engine"),
		EngineConfig:         engineConfigFromRequest(r),
		TerraformVersion:     r.FormValue("terraform_version"),
		AutoApprove:          r.FormValue("auto_approve") == "on",
		UpdateDrift:          r.Form.Has("drift_interval_minutes") || r.Form.Has("drift_enabled"),
		DriftEnabled:         driftEnabled,
		DriftIntervalMinutes: driftInterval,
		UpdateTTL:            r.Form.Has("ttl_hours") || r.Form.Has("auto_destroy"),
		TTLHours:             ttlHours,
		AutoDestroy:          autoDestroy,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !moved && (replacePolicies || r.Form.Has("tags_override") || r.Form.Has("tags")) {
		if err := u.editProject(r.Context(), id, func(p *project.Project) error {
			if replacePolicies {
				p.SetPolicyIDs(policyIDs)
			}
			if overrideTags {
				return p.SetTagOverride(tags)
			}
			p.ClearTagOverride()
			return nil
		}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if u.cfg.Contexts != nil {
		if err := u.cfg.Contexts.SetProjectContexts(r.Context(), ps.TenantID, id, parseContextIDs(r.Form["context_ids"])); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if u.cfg.CloudIntegrations != nil && r.Form.Has("cloud_integrations_present") {
		if err := u.cfg.CloudIntegrations.SetProjectIntegrations(r.Context(), ps.TenantID, id, parseCloudIntegrationIDs(r.Form["cloud_integration_ids"])); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if moved {
		var movedPolicyIDs []string
		var movedTags map[string]string
		if after, err := u.cfg.Projects.Get(r.Context(), id); err == nil && after != nil {
			if eff, err := u.effectiveProject(r.Context(), after); err == nil {
				movedPolicyIDs = eff.Snapshot().PolicyIDs
				movedTags = eff.Snapshot().EffectiveTags
			}
		}
		u.recordAudit(r, ps.TenantID, "workspace.moved", "workspace", string(id), ps.Name,
			fmt.Sprintf("Moved workspace %s to another folder", ps.Name),
			map[string]any{
				"from_folder_id": ps.FolderID, "to_folder_id": newFolderID,
				"policy_ids": movedPolicyIDs, "tags": project.FormatTags(movedTags),
			})
	}
	u.recordAudit(r, ps.TenantID, "workspace.updated", "workspace", string(id), ps.Name,
		fmt.Sprintf("Updated settings for workspace %s", ps.Name),
		map[string]any{
			"branch": r.FormValue("branch"), "workdir": r.FormValue("workdir"),
			"auto_approve": r.FormValue("auto_approve") == "on", "policy_ids": policyIDs,
			"drift_enabled": driftEnabled, "drift_interval_minutes": driftInterval,
			"ttl_hours": ttlHours, "auto_destroy": autoDestroy,
			"tags": project.FormatTags(tags),
		})
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
	var ownIDs []string
	if policyID != "" {
		ownIDs = []string{policyID}
	}
	if err := u.editProject(r.Context(), ps.ID, func(p *project.Project) error {
		p.SetPolicyIDs(ownIDs)
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.recordAudit(r, ps.TenantID, "workspace.policy_updated", "workspace", string(ps.ID), ps.Name,
		fmt.Sprintf("Updated approval policy on workspace %s to %s", ps.Name, firstNonEmpty(policyID, "none")),
		map[string]any{"approval_policy_id": policyID})
	http.Redirect(w, r, "/projects/"+string(ps.ID), http.StatusSeeOther)
}

func (u *ui) lock(w http.ResponseWriter, r *http.Request) {
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	nextLocked := !ps.Locked
	if err := u.cfg.Projects.SetLocked(r.Context(), ps.ID, nextLocked); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	action := "workspace.unlocked"
	verb := "Unlocked"
	if nextLocked {
		action = "workspace.locked"
		verb = "Locked"
	}
	u.recordAudit(r, ps.TenantID, action, "workspace", string(ps.ID), ps.Name,
		fmt.Sprintf("%s workspace %s", verb, ps.Name),
		map[string]any{"locked": nextLocked})
	http.Redirect(w, r, "/projects/"+string(ps.ID), http.StatusSeeOther)
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
	rs := opened.Snapshot()
	u.recordAudit(r, ps.TenantID, "run.triggered", "run", string(rs.ID), ps.Name,
		fmt.Sprintf("Manually triggered run %s on workspace %s (%s@%s)", rs.ID, ps.Name, ps.DefaultBranch, shortSHA(sha)),
		map[string]any{"project_id": string(ps.ID), "sha": sha, "branch": ps.DefaultBranch})
	http.Redirect(w, r, "/runs/"+string(rs.ID), http.StatusSeeOther)
}

func (u *ui) triggerDriftCheck(w http.ResponseWriter, r *http.Request) {
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
	opened, err := u.cfg.Runs.Open(r.Context(), runapp.OpenRequest{
		ProjectID:   ps.ID,
		Trigger:     run.TriggerDrift,
		SHA:         sha,
		Branch:      ps.DefaultBranch,
		Message:     "Drift detection check",
		Author:      u.actor(r),
		TriggeredBy: u.actor(r),
		PlanOnly:    true,
	})
	if err != nil {
		if errors.Is(err, runapp.ErrDuplicate) {
			http.Redirect(w, r, "/projects/"+string(ps.ID), http.StatusSeeOther)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rs := opened.Snapshot()
	u.recordAudit(r, ps.TenantID, "drift.checked", "run", string(rs.ID), ps.Name,
		fmt.Sprintf("Triggered drift detection run %s on workspace %s (%s@%s)", rs.ID, ps.Name, ps.DefaultBranch, shortSHA(sha)),
		map[string]any{"project_id": string(ps.ID), "sha": sha, "branch": ps.DefaultBranch})
	http.Redirect(w, r, "/runs/"+string(rs.ID), http.StatusSeeOther)
}

func (u *ui) extendTTL(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	extraHours, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("extra_hours")))
	if extraHours <= 0 {
		extraHours = ps.TTLHours
		if extraHours <= 0 {
			extraHours = 24
		}
	}
	if _, err := u.cfg.Projects.ExtendTTL(r.Context(), ps.ID, extraHours); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.recordAudit(r, ps.TenantID, "workspace.ttl_extended", "workspace", string(ps.ID), ps.Name,
		fmt.Sprintf("Extended TTL on workspace %s by +%dh", ps.Name, extraHours),
		map[string]any{"extra_hours": extraHours})
	ref := r.Header.Get("Referer")
	if strings.HasSuffix(ref, "/details") {
		http.Redirect(w, r, "/projects/"+string(ps.ID)+"/details", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+string(ps.ID), http.StatusSeeOther)
}

func (u *ui) triggerDestroy(w http.ResponseWriter, r *http.Request) {
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
	opened, err := u.cfg.Runs.Open(r.Context(), runapp.OpenRequest{
		ProjectID:   ps.ID,
		Trigger:     run.TriggerManual,
		SHA:         sha,
		Branch:      ps.DefaultBranch,
		Message:     "Destroy workspace resources",
		Author:      u.actor(r),
		TriggeredBy: u.actor(r),
		PlanOnly:    false,
		IsDestroy:   true,
	})
	if err != nil {
		if errors.Is(err, runapp.ErrDuplicate) {
			http.Redirect(w, r, "/projects/"+string(ps.ID), http.StatusSeeOther)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rs := opened.Snapshot()
	u.recordAudit(r, ps.TenantID, "workspace.destroy_triggered", "run", string(rs.ID), ps.Name,
		fmt.Sprintf("Triggered destroy run %s on workspace %s", rs.ID, ps.Name),
		map[string]any{"project_id": string(ps.ID), "sha": sha, "destroy": true})
	http.Redirect(w, r, "/runs/"+string(rs.ID), http.StatusSeeOther)
}

func (u *ui) forceUnlock(w http.ResponseWriter, r *http.Request) {
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	id := ps.ID
	if err := u.cfg.Projects.ForceUnlock(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.recordAudit(r, ps.TenantID, "workspace.force_unlocked", "workspace", string(id), ps.Name,
		fmt.Sprintf("Force-unlocked state on workspace %s", ps.Name), nil)
	http.Redirect(w, r, "/projects/"+string(id)+"/details", http.StatusSeeOther)
}

func (u *ui) delete(w http.ResponseWriter, r *http.Request) {
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	if err := u.cfg.Projects.Delete(r.Context(), ps.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.recordAudit(r, ps.TenantID, "workspace.deleted", "workspace", string(ps.ID), ps.Name,
		fmt.Sprintf("Deleted workspace %s (%s)", ps.Name, ps.Repo.String()), nil)
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
	rn, p, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	rs := rn.Snapshot()
	ps := p.Snapshot()
	id := rs.ID
	info := u.userInfo(r)
	if err := u.cfg.Runs.Approve(r.Context(), id, run.Approval{
		UserID: info.ID,
		Login:  firstNonEmpty(info.Login, u.actor(r)),
		Role:   info.Role,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.recordAudit(r, ps.TenantID, "run.approved", "run", string(id), ps.Name,
		fmt.Sprintf("Approved run %s on workspace %s", id, ps.Name),
		map[string]any{"project_id": string(ps.ID), "sha": rs.SHA})
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/runs/"+string(id))
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/runs/"+string(id), http.StatusSeeOther)
}

func (u *ui) unapprove(w http.ResponseWriter, r *http.Request) {
	rn, p, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	rs := rn.Snapshot()
	ps := p.Snapshot()
	id := rs.ID
	info := u.userInfo(r)
	if err := u.cfg.Runs.Unapprove(r.Context(), id, firstNonEmpty(info.Login, u.actor(r)), info.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.recordAudit(r, ps.TenantID, "run.unapproved", "run", string(id), ps.Name,
		fmt.Sprintf("Revoked approval for run %s on workspace %s", id, ps.Name),
		map[string]any{"project_id": string(ps.ID), "sha": rs.SHA})
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
	ps, err := u.effectiveSnapshot(r.Context(), p.Snapshot())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	policyIDs := effectivePolicyIDs(ps)
	if len(policyIDs) > 0 && u.cfg.Policies == nil {
		http.Error(w, "policy service not configured", http.StatusInternalServerError)
		return
	}
	if u.cfg.Policies != nil {
		resolved, rErr := u.cfg.Policies.ResolveForProject(r.Context(), ps.TenantID, policyIDs, ps.EffectiveTags)
		if rErr != nil {
			http.Error(w, "Policy evaluation failed: "+rErr.Error(), http.StatusForbidden)
			return
		}
		if len(resolved) > 0 {
			res, evalErr := u.cfg.Policies.EvaluateForProject(r.Context(), ps.TenantID, policyIDs, ps.EffectiveTags, rs.PlanJSON, u.userInfo(r), projectInfoFrom(ps), approvalsInfoFrom(rs.Approvals))
			if evalErr != nil {
				http.Error(w, "Policy evaluation failed: "+evalErr.Error(), http.StatusForbidden)
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
				u.recordAudit(r, ps.TenantID, "policy.enforcement_blocked", "run", string(rs.ID), ps.Name,
					fmt.Sprintf("Policy %s blocked confirmation of run %s on workspace %s (%s)", res.PolicyName, rs.ID, ps.Name, msg),
					map[string]any{"project_id": string(ps.ID), "policy_id": string(res.PolicyID), "status": string(res.Status), "deny": res.Deny})
				http.Error(w, msg, http.StatusForbidden)
				return
			}
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
	u.recordAudit(r, ps.TenantID, "run.confirmed", "run", string(id), ps.Name,
		fmt.Sprintf("Confirmed apply for run %s on workspace %s", id, ps.Name),
		map[string]any{"project_id": string(ps.ID), "sha": rs.SHA})
	if r.Header.Get("HX-Request") == "true" {
		u.history(w, r)
		return
	}
	http.Redirect(w, r, "/runs/"+string(id), http.StatusSeeOther)
}

func (u *ui) cancel(w http.ResponseWriter, r *http.Request) {
	rn, p, err := u.loadRun(w, r)
	if err != nil {
		return
	}
	rs := rn.Snapshot()
	ps := p.Snapshot()
	id := rs.ID
	if err := u.cfg.Runs.Cancel(r.Context(), id, u.actor(r)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.recordAudit(r, ps.TenantID, "run.cancelled", "run", string(id), ps.Name,
		fmt.Sprintf("Cancelled run %s on workspace %s", id, ps.Name),
		map[string]any{"project_id": string(ps.ID), "sha": rs.SHA})
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
	eff, err := u.effectiveProject(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, err
	}
	return eff, nil
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
	eff, err := u.effectiveProject(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil, err
	}
	return rn, eff, nil
}

func (u *ui) projectView(r *http.Request, ps project.Snapshot, runs []*run.Run, panel template.HTML) map[string]any {
	tracked := filterRuns(runs, isTracked)
	latest := latestSnapshot(tracked)
	meta, _ := u.cfg.States.Metadata(r.Context(), string(ps.ID))
	var estCost string
	if latest != nil {
		if latest.MonthlyCost > 0 {
			estCost = run.FormatMonthlyCost(latest.MonthlyCost)
		} else if len(latest.PlanJSON) > 0 {
			if parsed, err := run.ParsePlanJSON(latest.PlanJSON); err == nil && parsed != nil && parsed.Cost != nil && parsed.Cost.MonthlyCost > 0 {
				estCost = run.FormatMonthlyCost(parsed.Cost.MonthlyCost)
			}
		}
	}
	var crumbs []folderCrumb
	if panel != "" {
		if tree, err := u.folderTree(r.Context(), ps.TenantID, ps.FolderID); err == nil {
			crumbs = tree.crumbs(ps.FolderID)
		}
	}
	return map[string]any{
		"Breadcrumbs": crumbs,
		"ID":          string(ps.ID), "Name": ps.Name, "Locked": ps.Locked,
		"LatestLabel": latestLabel(latest), "LatestClass": latestClass(latest),
		"Meta": projectMeta(ps, latest, meta), "Empty": len(runs) == 0,
		"Panel":             panel,
		"Poll":              anyNonTerminal(runs),
		"EngineLabel":       project.EngineLabel(ps.Engine),
		"EngineIsTerraform": ps.Engine == "" || ps.Engine == project.EngineTerraform,
		"EngineIsHelm":      ps.Engine == project.EngineHelm,
		"DriftEnabled":      ps.DriftEnabled,
		"DriftStatus":       ps.DriftStatus,
		"DriftStatusLabel":  driftStatusLabel(ps.DriftStatus),
		"LastDriftCheckAt":  fmtTimePtr(ps.LastDriftCheckAt),
		"TTLHours":          ps.TTLHours,
		"AutoDestroy":       ps.AutoDestroy,
		"ExpiresAt":         fmtTimePtr(ps.ExpiresAt),
		"EstMonthlyCost":    estCost,
	}
}

func (u *ui) runView(_ *http.Request, ps project.Snapshot, rs run.Snapshot, evalView *runPolicyEvalView, panel template.HTML) map[string]any {
	return map[string]any{
		"ID": string(rs.ID), "ProjectID": string(ps.ID), "ProjectName": ps.Name,
		"Title": runTitle(rs), "Status": rs.Status.Label(), "StatusClass": statusClass(rs.Status),
		"Meta": runMeta(ps, rs), "ChangesLabel": template.HTML(changesLabel(rs)), "Panel": panel,
		"Poll":       !rs.Status.Terminal(),
		"PolicyEval": evalView,
	}
}

func projectMeta(ps project.Snapshot, latest *run.Snapshot, meta state.Metadata) metaView {
	m := metaView{
		Engine:           project.EngineLabel(ps.Engine),
		TerraformVersion: ps.TerraformVersion,
		Repo:             ps.Repo.String(),
		Workdir:          ps.WorkingDirectory,
		Branch:           ps.DefaultBranch,
		Tags:             project.TagList(ps.EffectiveTags),
		ShowManagedBy:    ps.Engine == "" || ps.Engine == project.EngineTerraform,
	}
	if ps.Engine != "" && ps.Engine != project.EngineTerraform {
		m.TerraformVersion = ""
	} else if latest != nil && latest.TerraformVersion != "" {
		m.TerraformVersion = latest.TerraformVersion
	}
	if latest != nil && latest.Branch != "" {
		m.Branch = latest.Branch
	}
	if latest != nil {
		m.SHA = shortSHA(latest.SHA)
	}
	if meta.Exists && !meta.UpdatedAt.IsZero() {
		m.StateUpdated = fmtTime(meta.UpdatedAt)
	}
	return m
}

func runMeta(ps project.Snapshot, rs run.Snapshot) metaView {
	m := metaView{
		TerraformVersion: rs.TerraformVersion, Repo: ps.Repo.String(), Workdir: ps.WorkingDirectory,
		Branch: firstNonEmpty(rs.Branch, ps.DefaultBranch), SHA: shortSHA(rs.SHA),
	}
	if repo := ps.Repo.String(); repo != "" {
		if rs.SHA != "" {
			m.SHAURL = "https://github.com/" + repo + "/commit/" + rs.SHA
		}
		if rs.PRNumber != nil && *rs.PRNumber > 0 {
			num := strconv.Itoa(*rs.PRNumber)
			m.PRNumber = "#" + num
			m.PRURL = "https://github.com/" + repo + "/pull/" + num
		}
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

func stateHistoryTab(id project.ID, meta state.Metadata, versions []state.VersionSummary, diff *state.StateDiff, poll bool) tabView {
	view := stateTab(id, meta, poll)
	current := ""
	if meta.Serial != nil {
		current = strconv.FormatInt(*meta.Serial, 10)
	}
	for _, v := range versions {
		serial := strconv.FormatInt(v.Serial, 10)
		changed := strings.Join(v.ChangedOutputs, ", ")
		runURL := ""
		if v.RunID != "" {
			runURL = "/projects/" + string(id) + "/runs/" + v.RunID
		}
		view.Versions = append(view.Versions, stateVersionView{
			Serial:    serial,
			Size:      fmt.Sprintf("%d B", v.Size),
			Resources: strconv.Itoa(v.ResourceCount),
			Actor:     v.Actor,
			RunID:     v.RunID,
			RunURL:    runURL,
			When:      fmtTime(v.CreatedAt),
			Changed:   changed,
			Current:   serial == current,
		})
	}
	view.CanRollback = meta.Exists && !meta.Locked && !poll
	if len(view.Versions) >= 2 {
		view.CompareFrom = view.Versions[len(view.Versions)-1].Serial
		view.CompareTo = view.Versions[0].Serial
	}
	if diff != nil {
		view.CompareFrom = strconv.FormatInt(diff.FromSerial, 10)
		view.CompareTo = strconv.FormatInt(diff.ToSerial, 10)
		dv := &stateDiffView{
			From:    view.CompareFrom,
			To:      view.CompareTo,
			Added:   diff.AddedCount,
			Changed: diff.ChangedCount,
			Removed: diff.RemovedCount,
			Unified: clipText(diff.UnifiedDiff, 20000),
		}
		for _, rc := range diff.ResourceChanges {
			dv.Resources = append(dv.Resources, stateChangeView{Name: rc.Address, Action: rc.Action})
		}
		for _, oc := range diff.OutputChanges {
			dv.Outputs = append(dv.Outputs, stateChangeView{
				Name: oc.Name, Action: oc.Action, Before: oc.Before, After: oc.After,
			})
		}
		view.Diff = dv
	}
	return view
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… truncated"
}

func stateTab(id project.ID, meta state.Metadata, poll bool) tabView {
	var summary *stateSummaryView
	if meta.Exists {
		serial := ""
		if meta.Serial != nil {
			serial = strconv.FormatInt(*meta.Serial, 10)
		}
		summary = &stateSummaryView{
			Time:   fmtTime(meta.UpdatedAt),
			Serial: serial,
			Size:   fmt.Sprintf("%d B", meta.Size),
			Locked: meta.Locked,
		}
	}
	return tabView{ProjectID: string(id), Poll: poll, State: summary}
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
	return s.Trigger == run.TriggerPush || s.Trigger == run.TriggerManual || s.Trigger == run.TriggerPipeline || s.Trigger == run.TriggerDrift || s.Trigger == run.TriggerTTL
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

func statusCategory(st run.Status) string {
	switch st {
	case run.StatusNeedsApproval:
		return "needs_approval"
	case run.StatusFailed:
		return "failed"
	case run.StatusQueued, run.StatusRunning:
		return "in_progress"
	case run.StatusSucceeded, run.StatusPlanned:
		return "finished"
	case run.StatusCancelled:
		return "cancelled"
	default:
		return "none"
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

func fmtTimePtr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return fmtTime(*t)
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
	Tags       []string
	TagsCSV    string
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
	filterName := strings.TrimSpace(r.URL.Query().Get("name"))
	filterTag := strings.TrimSpace(strings.Join(r.URL.Query()["tag"], ", "))
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
			tags := project.TagList(cs.Tags)
			if !matchesProjectFilter(cs.Name, "", tags, filterName, "", filterTag) {
				continue
			}
			rows = append(rows, contextRow{
				ID:         string(cs.ID),
				Name:       cs.Name,
				EntryCount: len(cs.Entries),
				UpdatedAt:  fmtTime(cs.UpdatedAt),
				Tags:       tags,
				TagsCSV:    strings.Join(tags, ","),
			})
		}
	}
	u.page(w, "Contexts", "contexts", map[string]any{
		"Contexts":   rows,
		"FilterName": filterName,
		"FilterTag":  filterTag,
		"KnownTags":  u.knownTags(r.Context(), tenantID),
		"CountAll":   len(rows),
	})
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
	tags, err := project.ParseTags(r.FormValue("tags"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, err := u.cfg.Contexts.Create(r.Context(), tenantID, r.FormValue("name"), tags)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.recordAudit(r, tenantID, "context.created", "context", string(c.Snapshot().ID), c.Snapshot().Name,
		fmt.Sprintf("Created context %s", c.Snapshot().Name),
		map[string]any{"tags": project.FormatTags(tags)})
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
			Kind:      tfcontext.KindLabel(e.Kind),
			Key:       e.Key,
			Value:     val,
			Secret:    e.Secret,
			UpdatedAt: fmtTime(e.UpdatedAt),
		})
	}
	u.page(w, cs.Name, "context", map[string]any{
		"ID":        string(cs.ID),
		"Name":      cs.Name,
		"Tags":      project.FormatTags(cs.Tags),
		"TagList":   project.TagList(cs.Tags),
		"KnownTags": u.knownTags(r.Context(), tenantID),
		"UpdatedAt": fmtTime(cs.UpdatedAt),
		"Entries":   entries,
	})
}

func (u *ui) updateContext(w http.ResponseWriter, r *http.Request) {
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
	tags, err := project.ParseTags(r.FormValue("tags"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := tfcontext.ID(r.PathValue("id"))
	if _, err := u.cfg.Contexts.Update(r.Context(), tenantID, id, r.FormValue("name"), tags); err != nil {
		if errors.Is(err, tfcontextapp.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.recordAudit(r, tenantID, "context.updated", "context", string(id), r.FormValue("name"),
		fmt.Sprintf("Updated context %s", r.FormValue("name")),
		map[string]any{"tags": project.FormatTags(tags)})
	http.Redirect(w, r, "/contexts/"+string(id), http.StatusSeeOther)
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
	kind := tfcontext.EntryKind(r.FormValue("kind"))
	key := r.FormValue("key")
	err := u.cfg.Contexts.UpsertEntry(
		r.Context(), tenantID, id,
		kind,
		key,
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
	u.recordAudit(r, tenantID, "context.entry_upserted", "context", string(id), key,
		fmt.Sprintf("Upserted %s entry %s in context %s (value redacted)", kind, key, id),
		map[string]any{"kind": string(kind), "key": key, "secret": kind != tfcontext.KindPlainEnv})
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
	entryID := r.PathValue("entryID")
	err := u.cfg.Contexts.DeleteEntry(r.Context(), tenantID, id, entryID)
	if err != nil {
		if errors.Is(err, tfcontextapp.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.recordAudit(r, tenantID, "context.entry_deleted", "context", string(id), entryID,
		fmt.Sprintf("Deleted entry %s from context %s", entryID, id),
		map[string]any{"entry_id": entryID})
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
	u.recordAudit(r, tenantID, "context.deleted", "context", string(id), string(id),
		fmt.Sprintf("Deleted context %s", id), nil)
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
	Tags        []string
	TagsCSV     string
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

func (u *ui) tagBoundPolicies(ctx context.Context, tenantID string, explicitIDs []policy.ID, projectTags map[string]string) []tagBoundPolicyView {
	if u.cfg.Policies == nil || tenantID == "" {
		return nil
	}
	resolved, err := u.cfg.Policies.ResolveForProject(ctx, tenantID, explicitIDs, projectTags)
	if err != nil {
		return nil
	}
	var out []tagBoundPolicyView
	for _, rp := range resolved {
		if rp.Source == "explicit" {
			continue
		}
		badge := "Tag: " + rp.MatchedTag
		if rp.Source == "global" {
			badge = "Global (" + rp.MatchedTag + ")"
		}
		out = append(out, tagBoundPolicyView{
			ID:    string(rp.Policy.ID()),
			Name:  rp.Policy.Name(),
			Badge: badge,
		})
	}
	return out
}

func policySourceBadge(source, matchedTag string, multi bool) string {
	switch source {
	case "tag-bound":
		return "Tag: " + matchedTag
	case "global":
		return "Global (" + matchedTag + ")"
	case "explicit":
		if multi {
			return "Explicit"
		}
	}
	return ""
}

func (u *ui) evaluateRunPolicy(r *http.Request, ps project.Snapshot, rs run.Snapshot) *runPolicyEvalView {
	approvable := rs.Status == run.StatusNeedsApproval || (rs.PlanOnly && rs.Status == run.StatusPlanned && rs.HasChanges())
	if !approvable || u.cfg.Policies == nil {
		return nil
	}
	ps, effErr := u.effectiveSnapshot(r.Context(), ps)
	if effErr != nil {
		return &runPolicyEvalView{
			Allowed: false,
			Deny:    []string{"Policy evaluation error: " + effErr.Error()},
		}
	}
	policyIDs := effectivePolicyIDs(ps)
	resolved, rErr := u.cfg.Policies.ResolveForProject(r.Context(), ps.TenantID, policyIDs, ps.EffectiveTags)
	if rErr == nil && len(resolved) == 0 {
		return nil
	}
	var firstID policy.ID
	if len(policyIDs) > 0 {
		firstID = policyIDs[0]
	}
	polName := u.policyName(r.Context(), ps.TenantID, firstID)
	if polName == "" {
		polName = string(firstID)
	}
	res, err := u.cfg.Policies.EvaluateForProject(r.Context(), ps.TenantID, policyIDs, ps.EffectiveTags, rs.PlanJSON, u.userInfo(r), projectInfoFrom(ps), approvalsInfoFrom(rs.Approvals))
	if err != nil {
		return &runPolicyEvalView{
			PolicyID:   string(firstID),
			PolicyName: polName,
			Allowed:    false,
			Deny:       []string{"Policy evaluation error: " + err.Error()},
		}
	}
	multi := len(res.Policies) > 1
	items := make([]runPolicyEvalItemView, 0, len(res.Policies))
	for _, it := range res.Policies {
		items = append(items, runPolicyEvalItemView{
			PolicyID:    string(it.PolicyID),
			PolicyName:  it.PolicyName,
			SourceBadge: policySourceBadge(it.Source, it.MatchedTag, multi),
			Allowed:     it.Allowed,
			Undecided:   it.Undecided,
			Deny:        it.Deny,
		})
	}
	return &runPolicyEvalView{
		PolicyID:    string(res.PolicyID),
		PolicyName:  firstNonEmpty(res.PolicyName, polName),
		SourceBadge: policySourceBadge(res.Source, res.MatchedTag, multi),
		Status:      string(res.Status),
		Allowed:     res.Allowed,
		Undecided:   res.Undecided,
		Deny:        res.Deny,
		Policies:    items,
	}
}

func (u *ui) policies(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	filterName := strings.TrimSpace(r.URL.Query().Get("name"))
	filterDescription := strings.TrimSpace(r.URL.Query().Get("description"))
	filterTag := strings.TrimSpace(strings.Join(r.URL.Query()["tag"], ", "))
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
			tags := project.TagList(ps.Tags)
			if !matchesProjectFilter(ps.Name, ps.Description, tags, filterName, filterDescription, filterTag) {
				continue
			}
			rows = append(rows, policyRow{
				ID:          string(ps.ID),
				Name:        ps.Name,
				Description: ps.Description,
				UpdatedAt:   fmtTime(ps.UpdatedAt),
				Tags:        tags,
				TagsCSV:     strings.Join(tags, ","),
			})
		}
	}
	u.page(w, "Policies", "policies", map[string]any{
		"Policies":          rows,
		"FilterName":        filterName,
		"FilterDescription": filterDescription,
		"FilterTag":         filterTag,
		"KnownTags":         u.knownTags(r.Context(), tenantID),
		"CountAll":          len(rows),
		"DefaultRego":       defaultSampleRego,
		"SampleRego":        defaultSampleRego,
		"SampleInputJSON":   defaultSampleInputJSON,
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
	tags, err := project.ParseTags(r.FormValue("tags"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := u.cfg.Policies.Create(r.Context(), policyapp.CreateInput{
		TenantID:    tenantID,
		Name:        r.FormValue("name"),
		Description: r.FormValue("description"),
		Rego:        r.FormValue("rego"),
		Tags:        tags,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ps := p.Snapshot()
	u.recordAudit(r, tenantID, "policy.created", "policy", string(ps.ID), ps.Name,
		fmt.Sprintf("Created OPA policy %s", ps.Name),
		map[string]any{"tags": project.FormatTags(tags)})
	http.Redirect(w, r, "/policies/"+string(ps.ID), http.StatusSeeOther)
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
		"Tags":            project.FormatTags(ps.Tags),
		"TagList":         project.TagList(ps.Tags),
		"KnownTags":       u.knownTags(r.Context(), tenantID),
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
	tags, err := project.ParseTags(r.FormValue("tags"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := policy.ID(r.PathValue("id"))
	if _, err := u.cfg.Policies.Update(r.Context(), policyapp.UpdateInput{
		ID:          id,
		TenantID:    tenantID,
		Name:        r.FormValue("name"),
		Description: r.FormValue("description"),
		Rego:        r.FormValue("rego"),
		Tags:        tags,
	}); err != nil {
		if errors.Is(err, policy.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.recordAudit(r, tenantID, "policy.updated", "policy", string(id), r.FormValue("name"),
		fmt.Sprintf("Updated OPA policy %s", r.FormValue("name")),
		map[string]any{"tags": project.FormatTags(tags)})
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
	polName := u.policyName(r.Context(), tenantID, id)
	if err := u.cfg.Policies.Delete(r.Context(), tenantID, id); err != nil {
		if errors.Is(err, policy.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Cannot delete policy (detach it from any projects first): "+err.Error(), http.StatusConflict)
		return
	}
	u.recordAudit(r, tenantID, "policy.deleted", "policy", string(id), firstNonEmpty(polName, string(id)),
		fmt.Sprintf("Deleted OPA policy %s", firstNonEmpty(polName, string(id))), nil)
	http.Redirect(w, r, "/policies", http.StatusSeeOther)
}

type depProjectChoice struct {
	ID   string
	Name string
}

type depMappingView struct {
	ID           string
	DepID        string
	OutputName   string
	EnvKey       string
	PreviewValue string
}

type upstreamDepView struct {
	ID                  string
	UpstreamProjectID   string
	UpstreamProjectName string
	TriggerAlways       bool
	AvailableOutputs    []string
	Mappings            []depMappingView
}

type downstreamDepView struct {
	ProjectID     string
	ProjectName   string
	TriggerAlways bool
	MappedOutputs []string
}

type dependenciesTabView struct {
	ProjectID         string
	CandidateProjects []depProjectChoice
	Upstream          []upstreamDepView
	Downstream        []downstreamDepView
}

func (u *ui) tabDependencies(w http.ResponseWriter, r *http.Request) {
	p, runs, err := u.loadProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	view := dependenciesTabView{ProjectID: string(ps.ID)}

	allProjects, err := u.cfg.Projects.List(r.Context(), ps.TenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	projNames := make(map[project.ID]string, len(allProjects))
	for _, item := range allProjects {
		is := item.Snapshot()
		projNames[is.ID] = is.Name
	}

	alreadyUpstream := make(map[project.ID]bool)
	if u.cfg.Dependencies != nil {
		upDeps, err := u.cfg.Dependencies.ListByProject(r.Context(), ps.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, d := range upDeps {
			ds := d.Snapshot()
			alreadyUpstream[ds.DependsOnProjectID] = true
			upName := projNames[ds.DependsOnProjectID]
			if upName == "" {
				upName = string(ds.DependsOnProjectID)
			}

			var outputs map[string]state.OutputValue
			if u.cfg.States != nil {
				outputs, _ = u.cfg.States.Outputs(r.Context(), string(ds.DependsOnProjectID))
			}
			var avail []string
			for k := range outputs {
				avail = append(avail, k)
			}
			sort.Strings(avail)

			mappings := make([]depMappingView, 0, len(ds.Mappings))
			for _, m := range ds.Mappings {
				preview := "(not in current state)"
				if outVal, ok := outputs[m.OutputName]; ok {
					if outVal.Sensitive {
						preview = "(sensitive)"
					} else {
						preview = outVal.EnvString
					}
				}
				mappings = append(mappings, depMappingView{
					ID:           m.ID,
					DepID:        string(ds.ID),
					OutputName:   m.OutputName,
					EnvKey:       m.EnvKey(),
					PreviewValue: preview,
				})
			}

			view.Upstream = append(view.Upstream, upstreamDepView{
				ID:                  string(ds.ID),
				UpstreamProjectID:   string(ds.DependsOnProjectID),
				UpstreamProjectName: upName,
				TriggerAlways:       ds.TriggerAlways,
				AvailableOutputs:    avail,
				Mappings:            mappings,
			})
		}

		downDeps, err := u.cfg.Dependencies.ListDownstream(r.Context(), ps.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, d := range downDeps {
			ds := d.Snapshot()
			downName := projNames[ds.ProjectID]
			if downName == "" {
				downName = string(ds.ProjectID)
			}
			var mapped []string
			for _, m := range ds.Mappings {
				mapped = append(mapped, m.OutputName)
			}
			view.Downstream = append(view.Downstream, downstreamDepView{
				ProjectID:     string(ds.ProjectID),
				ProjectName:   downName,
				TriggerAlways: ds.TriggerAlways,
				MappedOutputs: mapped,
			})
		}
	}

	for _, item := range allProjects {
		is := item.Snapshot()
		if is.ID == ps.ID || alreadyUpstream[is.ID] {
			continue
		}
		view.CandidateProjects = append(view.CandidateProjects, depProjectChoice{
			ID:   string(is.ID),
			Name: is.Name,
		})
	}

	if r.Header.Get("HX-Request") == "true" {
		u.fragment(w, "tab_dependencies", view)
		return
	}
	var panel bytes.Buffer
	if err := pages.ExecuteTemplate(&panel, "tab_dependencies", view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.page(w, ps.Name, "project", u.projectView(r, ps, runs, template.HTML(panel.String())))
}

func (u *ui) createDependency(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	if u.cfg.Dependencies == nil {
		http.Error(w, "dependencies service not configured", http.StatusInternalServerError)
		return
	}
	dependsOnID := project.ID(strings.TrimSpace(r.FormValue("depends_on_project_id")))
	triggerAlways := r.FormValue("trigger_always") == "on"
	if _, err := u.cfg.Dependencies.Create(r.Context(), ps.TenantID, ps.ID, dependsOnID, triggerAlways); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/projects/"+string(ps.ID)+"/tabs/dependencies", http.StatusSeeOther)
}

func (u *ui) updateDependencyTriggerMode(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	if u.cfg.Dependencies == nil {
		http.Error(w, "dependencies service not configured", http.StatusInternalServerError)
		return
	}
	depID := dependency.ID(r.PathValue("depID"))
	d, err := u.cfg.Dependencies.Get(r.Context(), ps.TenantID, depID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d == nil || d.Snapshot().ProjectID != ps.ID {
		http.NotFound(w, r)
		return
	}
	triggerAlways := r.FormValue("trigger_always") == "on"
	if err := u.cfg.Dependencies.SetTriggerAlways(r.Context(), ps.TenantID, depID, triggerAlways); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/projects/"+string(ps.ID)+"/tabs/dependencies", http.StatusSeeOther)
}

func (u *ui) deleteDependency(w http.ResponseWriter, r *http.Request) {
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	if u.cfg.Dependencies == nil {
		http.Error(w, "dependencies service not configured", http.StatusInternalServerError)
		return
	}
	depID := dependency.ID(r.PathValue("depID"))
	d, err := u.cfg.Dependencies.Get(r.Context(), ps.TenantID, depID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d == nil || d.Snapshot().ProjectID != ps.ID {
		http.NotFound(w, r)
		return
	}
	if err := u.cfg.Dependencies.Delete(r.Context(), ps.TenantID, depID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/projects/"+string(ps.ID)+"/tabs/dependencies", http.StatusSeeOther)
}

func (u *ui) addDependencyMapping(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	if u.cfg.Dependencies == nil {
		http.Error(w, "dependencies service not configured", http.StatusInternalServerError)
		return
	}
	depID := dependency.ID(r.PathValue("depID"))
	d, err := u.cfg.Dependencies.Get(r.Context(), ps.TenantID, depID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d == nil || d.Snapshot().ProjectID != ps.ID {
		http.NotFound(w, r)
		return
	}
	if err := u.cfg.Dependencies.AddMapping(r.Context(), ps.TenantID, depID, r.FormValue("output_name"), r.FormValue("input_name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/projects/"+string(ps.ID)+"/tabs/dependencies", http.StatusSeeOther)
}

func (u *ui) deleteDependencyMapping(w http.ResponseWriter, r *http.Request) {
	p, err := u.getProject(w, r)
	if err != nil {
		return
	}
	ps := p.Snapshot()
	if u.cfg.Dependencies == nil {
		http.Error(w, "dependencies service not configured", http.StatusInternalServerError)
		return
	}
	depID := dependency.ID(r.PathValue("depID"))
	d, err := u.cfg.Dependencies.Get(r.Context(), ps.TenantID, depID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d == nil || d.Snapshot().ProjectID != ps.ID {
		http.NotFound(w, r)
		return
	}
	if err := u.cfg.Dependencies.RemoveMapping(r.Context(), ps.TenantID, depID, r.PathValue("mapID")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.recordAudit(r, ps.TenantID, "dependency.updated", "dependency", string(depID), ps.Name,
		fmt.Sprintf("Removed output mapping from workspace %s dependency %s", ps.Name, depID), nil)
	http.Redirect(w, r, "/projects/"+string(ps.ID)+"/tabs/dependencies", http.StatusSeeOther)
}

type auditEntryRow struct {
	ID            string
	Timestamp     string
	ActorLogin    string
	ActorRole     string
	Action        string
	ActionClass   string
	ResourceType  string
	ResourceID    string
	ResourceLabel string
	ResourceURL   string
	Summary       string
	PrevHash      string
	EntryHash     string
	ShortHash     string
	Verified      bool
}

func auditActionClass(action string) string {
	switch {
	case strings.HasSuffix(action, ".deleted") || strings.Contains(action, "blocked") || strings.Contains(action, "cancelled") || strings.Contains(action, "force_unlocked"):
		return "pill-failed"
	case strings.HasSuffix(action, ".created") || strings.HasSuffix(action, ".confirmed") || strings.HasSuffix(action, ".approved"):
		return "pill-finished"
	case strings.Contains(action, "locked") || strings.Contains(action, "policy") || strings.Contains(action, "imported") || strings.Contains(action, "entry"):
		return "pill-unconfirmed"
	default:
		return "pill-running"
	}
}

func auditResourceURL(resourceType, resourceID string) string {
	if resourceID == "" {
		return ""
	}
	switch resourceType {
	case "workspace":
		return "/projects/" + resourceID
	case "state":
		return "/projects/" + resourceID + "/tabs/state"
	case "run":
		return "/runs/" + resourceID
	case "policy":
		return "/policies/" + resourceID
	case "context":
		return "/contexts/" + resourceID
	case "cloud_integration":
		return "/cloud-integrations/" + resourceID
	default:
		return ""
	}
}

func isSecurityOrGovernanceAudit(e audit.Snapshot) bool {
	switch e.ResourceType {
	case "policy", "context", "state", "cloud_integration":
		return true
	}
	return strings.Contains(e.Action, "lock") || strings.Contains(e.Action, "policy") || strings.Contains(e.Action, "approve") || strings.Contains(e.Action, "confirm") || strings.Contains(e.Action, "cloud_integration")
}

func (u *ui) parseAuditFilter(r *http.Request) (audit.Filter, string) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	resType := strings.TrimSpace(r.URL.Query().Get("resource_type"))
	actor := strings.TrimSpace(r.URL.Query().Get("actor"))
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	window := strings.TrimSpace(r.URL.Query().Get("window"))

	now := u.now()
	var since time.Time
	switch window {
	case "24h":
		since = now.Add(-24 * time.Hour)
	case "7d":
		since = now.Add(-7 * 24 * time.Hour)
	case "30d":
		since = now.Add(-30 * 24 * time.Hour)
	case "90d":
		since = now.Add(-90 * 24 * time.Hour)
	default:
		window = ""
	}
	return audit.Filter{
		ResourceType: resType,
		ActorLogin:   actor,
		Action:       action,
		Query:        q,
		Since:        since,
		Limit:        500,
	}, window
}

func (u *ui) auditLog(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	filter, window := u.parseAuditFilter(r)

	var allEntries []audit.Snapshot
	var filtered []audit.Snapshot
	if u.cfg.Audits != nil {
		var err error
		allEntries, err = u.cfg.Audits.List(r.Context(), tenantID, audit.Filter{Limit: 1000})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, e := range allEntries {
			if audit.MatchesFilter(e, filter) {
				filtered = append(filtered, e)
			}
		}
	}

	chainVerified := audit.VerifyChain(allEntries, true)
	var headHash string
	if len(allEntries) > 0 && len(allEntries[0].EntryHash) >= 12 {
		headHash = allEntries[0].EntryHash[:12]
	}

	actors := make(map[string]bool)
	securityEvents := 0
	for _, e := range allEntries {
		if e.ActorLogin != "" {
			actors[e.ActorLogin] = true
		}
		if isSecurityOrGovernanceAudit(e) {
			securityEvents++
		}
	}

	rows := make([]auditEntryRow, 0, len(filtered))
	for _, e := range filtered {
		shortHash := e.EntryHash
		if len(shortHash) > 12 {
			shortHash = shortHash[:12]
		}
		label := firstNonEmpty(firstNonEmpty(e.ResourceName, e.ResourceID), "-")
		rows = append(rows, auditEntryRow{
			ID:            string(e.ID),
			Timestamp:     e.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
			ActorLogin:    firstNonEmpty(e.ActorLogin, "system"),
			ActorRole:     e.ActorRole,
			Action:        e.Action,
			ActionClass:   auditActionClass(e.Action),
			ResourceType:  e.ResourceType,
			ResourceID:    e.ResourceID,
			ResourceLabel: label,
			ResourceURL:   auditResourceURL(e.ResourceType, e.ResourceID),
			Summary:       e.Summary,
			PrevHash:      e.PrevHash,
			EntryHash:     e.EntryHash,
			ShortHash:     shortHash,
			Verified:      audit.VerifyEntry(e),
		})
	}

	u.page(w, "Audit Log", "audit", map[string]any{
		"TenantID":       tenantID,
		"TotalEvents":    len(allEntries),
		"FilteredCount":  len(filtered),
		"ChainVerified":  chainVerified,
		"HeadHash":       headHash,
		"UniqueActors":   len(actors),
		"SecurityEvents": securityEvents,
		"Query":          filter.Query,
		"ResourceType":   filter.ResourceType,
		"Actor":          filter.ActorLogin,
		"Window":         window,
		"RawQuery":       r.URL.RawQuery,
		"Entries":        rows,
	})
}

func (u *ui) exportAuditLog(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	filter, _ := u.parseAuditFilter(r)
	filter.Limit = 5000

	var entries []audit.Snapshot
	if u.cfg.Audits != nil {
		var err error
		entries, err = u.cfg.Audits.List(r.Context(), tenantID, filter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"audit-log-%s.json\"", tenantID))
		payload := map[string]any{
			"tenant_id":      tenantID,
			"exported_at":    u.now().UTC().Format(time.RFC3339),
			"count":          len(entries),
			"chain_verified": audit.VerifyChain(entries, filter.ResourceType == "" && filter.ActorLogin == "" && filter.Query == "" && filter.Since.IsZero()),
			"entries":        entries,
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"audit-log-%s.csv\"", tenantID))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"id",
		"timestamp_utc",
		"tenant_id",
		"actor_id",
		"actor_login",
		"actor_role",
		"action",
		"resource_type",
		"resource_id",
		"resource_name",
		"summary",
		"ip_address",
		"prev_hash",
		"entry_hash",
	})
	for _, e := range entries {
		_ = cw.Write([]string{
			string(e.ID),
			e.CreatedAt.UTC().Format(time.RFC3339),
			e.TenantID,
			e.ActorID,
			e.ActorLogin,
			e.ActorRole,
			e.Action,
			e.ResourceType,
			e.ResourceID,
			e.ResourceName,
			e.Summary,
			e.IPAddress,
			e.PrevHash,
			e.EntryHash,
		})
	}
	cw.Flush()
}

func driftStatusLabel(status string) string {
	switch status {
	case project.DriftStatusSynced:
		return "In Sync"
	case project.DriftStatusDrifted:
		return "Drifted"
	case project.DriftStatusError:
		return "Drift Check Failed"
	default:
		return "Unknown"
	}
}

func ciProviderLabel(p string) string {
	switch strings.ToLower(p) {
	case cloudintegration.ProviderAWS:
		return "AWS"
	case cloudintegration.ProviderGCP:
		return "GCP"
	case cloudintegration.ProviderAzure:
		return "Azure"
	default:
		return strings.ToUpper(p)
	}
}

func ciAuthModeLabel(m string) string {
	switch strings.ToLower(m) {
	case cloudintegration.AuthModeAssumeRole, "sts_assume_role":
		return "AWS STS AssumeRole"
	default:
		return "OIDC Federation"
	}
}

func ciTargetSummary(cs cloudintegration.Snapshot) string {
	switch cs.Provider {
	case cloudintegration.ProviderGCP:
		if cs.GCPServiceAccountEmail != "" {
			return cs.GCPServiceAccountEmail
		}
		return cs.GCPWorkloadIdentityProvider
	case cloudintegration.ProviderAzure:
		return cs.AzureClientID
	default:
		return cs.RoleARN
	}
}

func normalizeCIAuthMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "sts_assume_role", cloudintegration.AuthModeAssumeRole:
		return cloudintegration.AuthModeAssumeRole
	default:
		return cloudintegration.AuthModeOIDC
	}
}

type cloudIntegrationChoice struct {
	ID            string
	Name          string
	Provider      string
	ProviderLabel string
	TargetSummary string
	Attached      bool
}

type cloudIntegrationRow struct {
	ID               string
	Name             string
	Provider         string
	ProviderLabel    string
	AuthMethod       string
	AuthMethodLabel  string
	TargetSummary    string
	Region           string
	SessionDurationS int
	Tags             []string
	TagsCSV          string
	UpdatedAt        string
}

func (u *ui) cloudIntegrationChoices(ctx context.Context, tenantID string, projectID project.ID) []cloudIntegrationChoice {
	if u.cfg.CloudIntegrations == nil || tenantID == "" {
		return nil
	}
	all, err := u.cfg.CloudIntegrations.List(ctx, tenantID)
	if err != nil {
		return nil
	}
	attached := make(map[cloudintegration.ID]bool)
	if projectID != "" {
		if list, err := u.cfg.CloudIntegrations.ListByProject(ctx, projectID); err == nil {
			for _, ci := range list {
				attached[ci.Snapshot().ID] = true
			}
		}
	}
	out := make([]cloudIntegrationChoice, 0, len(all))
	for _, ci := range all {
		cs := ci.Snapshot()
		out = append(out, cloudIntegrationChoice{
			ID:            string(cs.ID),
			Name:          cs.Name,
			Provider:      cs.Provider,
			ProviderLabel: ciProviderLabel(cs.Provider),
			TargetSummary: ciTargetSummary(cs),
			Attached:      attached[cs.ID],
		})
	}
	return out
}

func (u *ui) tagBoundCloudIntegrations(ctx context.Context, tenantID string, projectID project.ID, projectTags map[string]string) []cloudIntegrationChoice {
	if u.cfg.CloudIntegrations == nil || tenantID == "" || len(projectTags) == 0 {
		return nil
	}
	all, err := u.cfg.CloudIntegrations.List(ctx, tenantID)
	if err != nil {
		return nil
	}
	attached := make(map[cloudintegration.ID]bool)
	if projectID != "" {
		if list, err := u.cfg.CloudIntegrations.ListByProject(ctx, projectID); err == nil {
			for _, ci := range list {
				attached[ci.Snapshot().ID] = true
			}
		}
	}
	var out []cloudIntegrationChoice
	for _, ci := range all {
		cs := ci.Snapshot()
		if attached[cs.ID] || len(cs.Tags) == 0 {
			continue
		}
		if matched, _ := ci.MatchesProjectTags(projectTags); matched {
			out = append(out, cloudIntegrationChoice{
				ID:            string(cs.ID),
				Name:          cs.Name,
				Provider:      cs.Provider,
				ProviderLabel: ciProviderLabel(cs.Provider),
				TargetSummary: ciTargetSummary(cs),
			})
		}
	}
	return out
}

func parseCloudIntegrationIDs(raw []string) []cloudintegration.ID {
	out := make([]cloudintegration.ID, 0, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id != "" {
			out = append(out, cloudintegration.ID(id))
		}
	}
	return out
}

func (u *ui) cloudIntegrations(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	filterName := strings.TrimSpace(r.URL.Query().Get("name"))
	filterTag := strings.TrimSpace(strings.Join(r.URL.Query()["tag"], ", "))
	var rows []cloudIntegrationRow
	var issuer, audience string
	if u.cfg.CloudIntegrations != nil {
		issuer = u.cfg.CloudIntegrations.Issuer()
		audience = cloudintegration.DefaultAWSAudience
		items, err := u.cfg.CloudIntegrations.List(r.Context(), tenantID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows = make([]cloudIntegrationRow, 0, len(items))
		for _, ci := range items {
			cs := ci.Snapshot()
			tags := project.TagList(cs.Tags)
			if !matchesProjectFilter(cs.Name, "", tags, filterName, "", filterTag) {
				continue
			}
			rows = append(rows, cloudIntegrationRow{
				ID:               string(cs.ID),
				Name:             cs.Name,
				Provider:         cs.Provider,
				ProviderLabel:    ciProviderLabel(cs.Provider),
				AuthMethod:       cs.AuthMode,
				AuthMethodLabel:  ciAuthModeLabel(cs.AuthMode),
				TargetSummary:    ciTargetSummary(cs),
				Region:           cs.Region,
				SessionDurationS: cs.SessionDurationSeconds,
				Tags:             tags,
				TagsCSV:          strings.Join(tags, ","),
				UpdatedAt:        fmtTime(cs.UpdatedAt),
			})
		}
	}
	u.page(w, "Cloud Integrations", "cloud_integrations", map[string]any{
		"Integrations": rows,
		"FilterName":   filterName,
		"FilterTag":    filterTag,
		"KnownTags":    u.knownTags(r.Context(), tenantID),
		"CountAll":     len(rows),
		"OIDCIssuer":   issuer,
		"OIDCAudience": audience,
	})
}

func (u *ui) createCloudIntegration(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.CloudIntegrations == nil {
		http.Error(w, "cloud integrations service not configured", http.StatusInternalServerError)
		return
	}
	tags, err := project.ParseTags(r.FormValue("tags"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	durationS, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("session_duration_seconds")))
	authMode := normalizeCIAuthMode(firstNonEmpty(r.FormValue("auth_mode"), r.FormValue("auth_method")))
	ci, err := u.cfg.CloudIntegrations.Create(r.Context(), cloudintegration.Snapshot{
		TenantID:                    tenantID,
		Name:                        r.FormValue("name"),
		Provider:                    r.FormValue("provider"),
		AuthMode:                    authMode,
		RoleARN:                     r.FormValue("role_arn"),
		Region:                      r.FormValue("region"),
		SessionDurationSeconds:      durationS,
		ExternalID:                  r.FormValue("external_id"),
		AccessKeyID:                 r.FormValue("access_key_id"),
		SecretAccessKey:             r.FormValue("secret_access_key"),
		Audience:                    r.FormValue("audience"),
		GCPWorkloadIdentityProvider: r.FormValue("gcp_workload_identity_provider"),
		GCPServiceAccountEmail:      r.FormValue("gcp_service_account_email"),
		AzureTenantID:               r.FormValue("azure_tenant_id"),
		AzureClientID:               r.FormValue("azure_client_id"),
		AzureSubscriptionID:         r.FormValue("azure_subscription_id"),
		Tags:                        tags,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cs := ci.Snapshot()
	u.recordAudit(r, tenantID, "cloud_integration.created", "cloud_integration", string(cs.ID), cs.Name,
		fmt.Sprintf("Created %s cloud integration %s (%s)", ciProviderLabel(cs.Provider), cs.Name, ciTargetSummary(cs)),
		map[string]any{"provider": cs.Provider, "auth_mode": cs.AuthMode, "target": ciTargetSummary(cs), "tags": project.FormatTags(tags)})
	http.Redirect(w, r, "/cloud-integrations/"+string(cs.ID), http.StatusSeeOther)
}

func (u *ui) cloudIntegrationDetails(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.CloudIntegrations == nil {
		http.NotFound(w, r)
		return
	}
	ci, err := u.cfg.CloudIntegrations.Get(r.Context(), tenantID, cloudintegration.ID(r.PathValue("id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ci == nil {
		http.NotFound(w, r)
		return
	}
	cs := ci.Snapshot()
	u.page(w, cs.Name, "cloud_integration", map[string]any{
		"ID":                          string(cs.ID),
		"Name":                        cs.Name,
		"Provider":                    cs.Provider,
		"ProviderLabel":               ciProviderLabel(cs.Provider),
		"AuthMethod":                  cs.AuthMode,
		"AuthMethodLabel":             ciAuthModeLabel(cs.AuthMode),
		"RoleARN":                     cs.RoleARN,
		"Region":                      cs.Region,
		"SessionDurationS":            cs.SessionDurationSeconds,
		"ExternalID":                  cs.ExternalID,
		"Audience":                    cs.Audience,
		"GCPWorkloadIdentityProvider": cs.GCPWorkloadIdentityProvider,
		"GCPServiceAccountEmail":      cs.GCPServiceAccountEmail,
		"GCPProjectID":                "",
		"AzureTenantID":               cs.AzureTenantID,
		"AzureClientID":               cs.AzureClientID,
		"AzureSubscriptionID":         cs.AzureSubscriptionID,
		"Tags":                        project.FormatTags(cs.Tags),
		"TagList":                     project.TagList(cs.Tags),
		"KnownTags":                   u.knownTags(r.Context(), tenantID),
		"UpdatedAt":                   fmtTime(cs.UpdatedAt),
		"OIDCIssuer":                  u.cfg.CloudIntegrations.Issuer(),
		"OIDCAudience":                firstNonEmpty(cs.Audience, cloudintegration.DefaultAWSAudience),
	})
}

func (u *ui) updateCloudIntegration(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.CloudIntegrations == nil {
		http.Error(w, "cloud integrations service not configured", http.StatusInternalServerError)
		return
	}
	tags, err := project.ParseTags(r.FormValue("tags"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := cloudintegration.ID(r.PathValue("id"))
	durationS, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("session_duration_seconds")))
	authMode := normalizeCIAuthMode(firstNonEmpty(r.FormValue("auth_mode"), r.FormValue("auth_method")))
	ci, err := u.cfg.CloudIntegrations.Update(r.Context(), tenantID, id, cloudintegration.Snapshot{
		Name:                        r.FormValue("name"),
		Provider:                    r.FormValue("provider"),
		AuthMode:                    authMode,
		RoleARN:                     r.FormValue("role_arn"),
		Region:                      r.FormValue("region"),
		SessionDurationSeconds:      durationS,
		ExternalID:                  r.FormValue("external_id"),
		AccessKeyID:                 r.FormValue("access_key_id"),
		SecretAccessKey:             r.FormValue("secret_access_key"),
		Audience:                    r.FormValue("audience"),
		GCPWorkloadIdentityProvider: r.FormValue("gcp_workload_identity_provider"),
		GCPServiceAccountEmail:      r.FormValue("gcp_service_account_email"),
		AzureTenantID:               r.FormValue("azure_tenant_id"),
		AzureClientID:               r.FormValue("azure_client_id"),
		AzureSubscriptionID:         r.FormValue("azure_subscription_id"),
		Tags:                        tags,
	})
	if err != nil {
		if errors.Is(err, cloudintegrationapp.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cs := ci.Snapshot()
	u.recordAudit(r, tenantID, "cloud_integration.updated", "cloud_integration", string(cs.ID), cs.Name,
		fmt.Sprintf("Updated %s cloud integration %s", ciProviderLabel(cs.Provider), cs.Name),
		map[string]any{"provider": cs.Provider, "auth_mode": cs.AuthMode, "target": ciTargetSummary(cs), "tags": project.FormatTags(tags)})
	http.Redirect(w, r, "/cloud-integrations/"+string(cs.ID), http.StatusSeeOther)
}

func (u *ui) deleteCloudIntegration(w http.ResponseWriter, r *http.Request) {
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if u.cfg.CloudIntegrations == nil {
		http.Error(w, "cloud integrations service not configured", http.StatusInternalServerError)
		return
	}
	id := cloudintegration.ID(r.PathValue("id"))
	existing, _ := u.cfg.CloudIntegrations.Get(r.Context(), tenantID, id)
	if err := u.cfg.CloudIntegrations.Delete(r.Context(), tenantID, id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := string(id)
	if existing != nil {
		name = existing.Snapshot().Name
	}
	u.recordAudit(r, tenantID, "cloud_integration.deleted", "cloud_integration", string(id), name,
		fmt.Sprintf("Deleted cloud integration %s", name), nil)
	http.Redirect(w, r, "/cloud-integrations", http.StatusSeeOther)
}

// effectiveSnapshot resolves stacked folder policies and the nearest tags for a stored project.
// Without a folder service the project is treated as having no folder: only its own ids and tags apply.
func (u *ui) effectiveSnapshot(ctx context.Context, ps project.Snapshot) (project.Snapshot, error) {
	if u.cfg.Folders != nil {
		return u.cfg.Folders.Effective(ctx, ps)
	}
	ps.EffectiveTags = map[string]string{}
	for k, v := range ps.Tags {
		ps.EffectiveTags[k] = v
	}
	ids := append([]string(nil), ps.PolicyIDs...)
	sort.Strings(ids)
	ps.EffectivePolicies = nil
	for _, id := range ids {
		ps.EffectivePolicies = append(ps.EffectivePolicies, project.EffectivePolicy{PolicyID: id, Source: "project"})
	}
	return ps, nil
}

func (u *ui) effectiveProject(ctx context.Context, p *project.Project) (*project.Project, error) {
	eff, err := u.effectiveSnapshot(ctx, p.Snapshot())
	if err != nil {
		return nil, err
	}
	return project.Restore(eff), nil
}

func (u *ui) effectiveList(ctx context.Context, tenantID string) ([]*project.Project, error) {
	items, err := u.cfg.Projects.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]*project.Project, 0, len(items))
	for _, p := range items {
		eff, err := u.effectiveProject(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, eff)
	}
	return out, nil
}

func effectivePolicyIDs(ps project.Snapshot) []policy.ID {
	if len(ps.EffectivePolicies) == 0 {
		return nil
	}
	out := make([]policy.ID, 0, len(ps.EffectivePolicies))
	for _, ep := range ps.EffectivePolicies {
		out = append(out, policy.ID(ep.PolicyID))
	}
	return out
}

// editProject loads the stored project, applies fn, and saves it.
func (u *ui) editProject(ctx context.Context, id project.ID, fn func(*project.Project) error) error {
	p, err := u.cfg.Projects.Projects.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("project %s not found", id)
	}
	if err := fn(p); err != nil {
		return err
	}
	return u.cfg.Projects.Projects.Save(ctx, p)
}

func (u *ui) validPolicyIDs(r *http.Request, tenantID string, ids []string) bool {
	if u.cfg.Policies == nil {
		return true
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := u.cfg.Policies.Get(r.Context(), tenantID, policy.ID(id)); err != nil {
			return false
		}
	}
	return true
}

func (u *ui) policyChoicesFor(ctx context.Context, tenantID string, selected []string) []policyChoice {
	chosen := make(map[string]bool, len(selected))
	for _, id := range selected {
		chosen[strings.TrimSpace(id)] = true
	}
	out := u.policyChoices(ctx, tenantID, "")
	for i := range out {
		out[i].Selected = chosen[out[i].ID]
	}
	return out
}

// folderFields feeds the shared folder policy and tag inputs.
type folderFields struct {
	Policies          []policyChoice
	PassDown, TagsSet bool
	IsRoot            bool
	Tags              string
}

type folderNode struct {
	ID, ParentID, Name, Path, Label string
	Depth, Indent                   int
	Selected, IsRoot                bool
	PassDown, TagsSet               bool
	Tags                            string
	PolicyIDs                       []string
}

type folderCrumb struct{ ID, Name string }

type folderTreeView struct {
	nodes    []folderNode
	selected folderNode
	paths    map[string]string
	byID     map[string]folderNode
}

func (t folderTreeView) crumbs(folderID string) []folderCrumb {
	var chain []folderCrumb
	seen := map[string]bool{}
	for id := folderID; id != "" && !seen[id]; {
		seen[id] = true
		n, ok := t.byID[id]
		if !ok {
			break
		}
		chain = append([]folderCrumb{{ID: n.ID, Name: n.Name}}, chain...)
		id = n.ParentID
	}
	return chain
}

// folderTree lists the tenant's folders depth first. An unknown or empty selection picks the root.
func (u *ui) folderTree(ctx context.Context, tenantID, selectedID string) (folderTreeView, error) {
	view := folderTreeView{paths: map[string]string{}, byID: map[string]folderNode{}}
	if u.cfg.Folders == nil || tenantID == "" {
		return view, nil
	}
	root, err := u.cfg.Folders.EnsureRoot(ctx, tenantID)
	if err != nil {
		return view, err
	}
	folders, err := u.cfg.Folders.List(ctx, tenantID)
	if err != nil {
		return view, err
	}
	children := map[string][]folder.Snapshot{}
	byID := map[string]folder.Snapshot{}
	for _, f := range folders {
		byID[f.ID] = f
		children[f.ParentID] = append(children[f.ParentID], f)
	}
	for _, list := range children {
		sort.Slice(list, func(i, j int) bool {
			a, b := strings.ToLower(list[i].Name), strings.ToLower(list[j].Name)
			if a != b {
				return a < b
			}
			return list[i].ID < list[j].ID
		})
	}
	if _, ok := byID[selectedID]; !ok {
		selectedID = root.ID
	}
	var walk func(f folder.Snapshot, parentPath string, depth int)
	walk = func(f folder.Snapshot, parentPath string, depth int) {
		path := f.ID
		if parentPath != "" {
			path = parentPath + "/" + f.ID
		}
		node := folderNode{
			ID: f.ID, ParentID: f.ParentID, Name: f.Name, Path: path,
			Label: strings.Repeat("\u2014 ", depth) + f.Name,
			Depth: depth, Indent: depth * 16,
			Selected: f.ID == selectedID, IsRoot: f.ParentID == "",
			PassDown: f.PassDown, TagsSet: f.TagsSet,
			Tags: project.FormatTags(f.Tags), PolicyIDs: append([]string(nil), f.PolicyIDs...),
		}
		view.nodes = append(view.nodes, node)
		view.paths[f.ID] = path
		view.byID[f.ID] = node
		if node.Selected {
			view.selected = node
		}
		for _, child := range children[f.ID] {
			walk(child, path, depth+1)
		}
	}
	for _, top := range children[""] {
		walk(top, "", 0)
	}
	return view, nil
}

type appliedPolicy struct {
	ID, Name, Label string
}

func (u *ui) appliedFromEffective(ctx context.Context, ps project.Snapshot) []appliedPolicy {
	var out []appliedPolicy
	for _, ep := range ps.EffectivePolicies {
		if ep.Source == "project" {
			continue
		}
		label := "Passed down"
		if ep.Source == ps.FolderID {
			label = "This folder"
		}
		out = append(out, appliedPolicy{
			ID:    ep.PolicyID,
			Name:  firstNonEmpty(u.policyName(ctx, ps.TenantID, policy.ID(ep.PolicyID)), ep.PolicyID),
			Label: label,
		})
	}
	return out
}

func (u *ui) appliedFolderPolicies(ctx context.Context, ps project.Snapshot) []appliedPolicy {
	return u.appliedFromEffective(ctx, ps)
}

// applicableFolderPolicies lists what a new project in the folder would inherit.
func (u *ui) applicableFolderPolicies(ctx context.Context, tenantID, folderID string) []appliedPolicy {
	if u.cfg.Folders == nil || folderID == "" {
		return nil
	}
	eff, err := u.cfg.Folders.Effective(ctx, project.Snapshot{TenantID: tenantID, FolderID: folderID})
	if err != nil {
		return nil
	}
	return u.appliedFromEffective(ctx, eff)
}

func (u *ui) folderEffectiveTags(ctx context.Context, tenantID, folderID string) map[string]string {
	if u.cfg.Folders == nil || folderID == "" {
		return nil
	}
	eff, err := u.cfg.Folders.Effective(ctx, project.Snapshot{TenantID: tenantID, FolderID: folderID})
	if err != nil {
		return nil
	}
	return eff.EffectiveTags
}

var notFoundError = regexp.MustCompile(`^(folder|project) \S+ not found$`)

var folderDomainErrors = map[string]bool{
	"folder name is required":                     true,
	"folder name cannot contain a slash":          true,
	"folder name contains invalid characters":     true,
	"folder name is too long":                     true,
	"a folder with that name already exists here": true,
	"the root folder cannot be deleted":           true,
	"the root folder cannot be moved":             true,
	"the root folder must set its own tags":       true,
	"folder cannot be moved under itself":         true,
	"folder depth cannot exceed 8":                true,
}

func (u *ui) folderError(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case notFoundError.MatchString(msg):
		http.Error(w, msg, http.StatusNotFound)
	case folderDomainErrors[msg] || errors.Is(err, project.ErrInvalidTag):
		http.Error(w, msg, http.StatusBadRequest)
	default:
		http.Error(w, msg, http.StatusInternalServerError)
	}
}

func (u *ui) folderTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	_, tenantID, _, ok := SessionFrom(r.Context())
	if !ok || tenantID == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return "", false
	}
	if u.cfg.Folders == nil {
		http.Error(w, "folder service not configured", http.StatusInternalServerError)
		return "", false
	}
	return tenantID, true
}

type folderSettingsForm struct {
	name      string
	passDown  bool
	policyIDs []string
	tagsSet   bool
	tags      map[string]string
}

func (u *ui) parseFolderSettings(w http.ResponseWriter, r *http.Request, tenantID string) (folderSettingsForm, bool) {
	f := folderSettingsForm{
		name:      r.FormValue("name"),
		passDown:  r.FormValue("pass_down") == "on",
		policyIDs: r.Form["policy_id"],
		tagsSet:   r.FormValue("tags_set") == "on",
	}
	if !u.validPolicyIDs(r, tenantID, f.policyIDs) {
		http.Error(w, "invalid policy_id", http.StatusBadRequest)
		return f, false
	}
	if f.tagsSet {
		tags, err := project.ParseTags(r.FormValue("tags"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return f, false
		}
		f.tags = tags
	}
	return f, true
}

func folderRedirect(id string) string { return "/projects?folder=" + url.QueryEscape(id) }

func (u *ui) createFolder(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := u.folderTenant(w, r)
	if !ok {
		return
	}
	form, ok := u.parseFolderSettings(w, r, tenantID)
	if !ok {
		return
	}
	in := folderapp.CreateInput{
		TenantID: tenantID, ParentID: strings.TrimSpace(r.FormValue("parent_id")), Name: form.name,
		TagsSet: form.tagsSet, Tags: form.tags,
	}
	in.PassDown = form.passDown
	in.PolicyIDs = form.policyIDs
	created, err := u.cfg.Folders.Create(r.Context(), in)
	if err != nil {
		u.folderError(w, err)
		return
	}
	u.recordAudit(r, tenantID, "folder.created", "folder", created.ID, created.Name,
		fmt.Sprintf("Created folder %s", created.Name),
		map[string]any{"parent_id": created.ParentID, "pass_down": created.PassDown, "policy_ids": created.PolicyIDs, "tags": project.FormatTags(created.Tags)})
	http.Redirect(w, r, folderRedirect(created.ID), http.StatusSeeOther)
}

func (u *ui) updateFolder(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := u.folderTenant(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	before, err := u.cfg.Folders.Get(r.Context(), tenantID, id)
	if err != nil {
		u.folderError(w, err)
		return
	}
	form, ok := u.parseFolderSettings(w, r, tenantID)
	if !ok {
		return
	}
	updateTags := r.Form.Has("tags_field")
	updated, err := u.cfg.Folders.UpdateSettings(r.Context(), folderapp.SettingsInput{
		TenantID: tenantID, ID: id, Name: form.name,
		PassDown: form.passDown, PolicyIDs: form.policyIDs, TagsSet: form.tagsSet, Tags: form.tags,
		UpdateTags: updateTags,
	})
	if err != nil {
		u.folderError(w, err)
		return
	}
	if updated.Name != before.Name {
		u.recordAudit(r, tenantID, "folder.renamed", "folder", updated.ID, updated.Name,
			fmt.Sprintf("Renamed folder %s to %s", before.Name, updated.Name),
			map[string]any{"from": before.Name, "to": updated.Name})
	}
	if updateTags {
		u.recordAudit(r, tenantID, "folder.settings_updated", "folder", updated.ID, updated.Name,
			fmt.Sprintf("Updated settings for folder %s", updated.Name),
			map[string]any{"pass_down": updated.PassDown, "policy_ids": updated.PolicyIDs, "tags_set": updated.TagsSet, "tags": project.FormatTags(updated.Tags)})
	}
	http.Redirect(w, r, folderRedirect(updated.ID), http.StatusSeeOther)
}

func (u *ui) moveFolder(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := u.folderTenant(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	before, err := u.cfg.Folders.Get(r.Context(), tenantID, id)
	if err != nil {
		u.folderError(w, err)
		return
	}
	parentID := strings.TrimSpace(r.FormValue("parent_id"))
	if err := u.cfg.Folders.Move(r.Context(), tenantID, id, parentID); err != nil {
		u.folderError(w, err)
		return
	}
	u.recordAudit(r, tenantID, "folder.moved", "folder", before.ID, before.Name,
		fmt.Sprintf("Moved folder %s", before.Name),
		map[string]any{"from_parent_id": before.ParentID, "to_parent_id": parentID})
	http.Redirect(w, r, folderRedirect(id), http.StatusSeeOther)
}

func (u *ui) deleteFolder(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := u.folderTenant(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	before, err := u.cfg.Folders.Get(r.Context(), tenantID, id)
	if err != nil {
		u.folderError(w, err)
		return
	}
	parentID, err := u.cfg.Folders.Delete(r.Context(), tenantID, id)
	if err != nil {
		u.folderError(w, err)
		return
	}
	u.recordAudit(r, tenantID, "folder.deleted", "folder", before.ID, before.Name,
		fmt.Sprintf("Deleted folder %s", before.Name),
		map[string]any{"parent_id": parentID})
	http.Redirect(w, r, folderRedirect(parentID), http.StatusSeeOther)
}
