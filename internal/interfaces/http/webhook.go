package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"infra-guy/internal/application/installation"
	runapp "infra-guy/internal/application/run"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
)

type opener interface {
	Open(context.Context, runapp.OpenRequest) (*run.Run, error)
}

type chatOpsRunner interface {
	opener
	ListByProject(ctx context.Context, projectID project.ID) ([]*run.Run, error)
	Approve(ctx context.Context, id run.ID, a run.Approval) error
	Confirm(ctx context.Context, id run.ID, actor string) error
}

func validSignature(secret, header string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func (a *api) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if a.webhookSecret == "" || !validSignature(a.webhookSecret, r.Header.Get("X-Hub-Signature-256"), body) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		w.WriteHeader(http.StatusOK)
		return
	}

	var env struct {
		Action       string `json:"action"`
		Installation struct {
			ID          int64   `json:"id"`
			Suspended   bool    `json:"suspended"`
			SuspendedAt *string `json:"suspended_at"`
			Account     struct {
				ID    int64  `json:"id"`
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
		} `json:"installation"`
		Repository struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			FullName string `json:"full_name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	if env.Installation.ID != 0 && a.installations != nil {
		ev := installation.InstallationEvent{
			Action:         env.Action,
			InstallationID: env.Installation.ID,
			AccountID:      env.Installation.Account.ID,
			AccountLogin:   env.Installation.Account.Login,
			AccountType:    env.Installation.Account.Type,
			Suspended:      env.Installation.Suspended || (env.Installation.SuspendedAt != nil && *env.Installation.SuspendedAt != ""),
		}
		if err := a.installations.Apply(r.Context(), event, ev); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	if event == "installation" || a.installations == nil || env.Installation.ID == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	inst, err := a.installations.Installation(r.Context(), env.Installation.ID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if inst == nil || !inst.Active {
		w.WriteHeader(http.StatusOK)
		return
	}

	if env.Repository.ID == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	ownerLogin := env.Repository.Owner.Login
	repoName := env.Repository.Name
	if ownerLogin == "" && strings.Contains(env.Repository.FullName, "/") {
		parts := strings.Split(env.Repository.FullName, "/")
		ownerLogin = parts[0]
		if repoName == "" {
			repoName = parts[1]
		}
	}
	var syncOwner, syncName string
	if event == "push" || event == "pull_request" || (event == "repository" && env.Action == "renamed") {
		syncOwner, syncName = ownerLogin, repoName
	}
	matched, err := a.projects.SyncAndFindByRepo(r.Context(), inst.TenantID, env.Repository.ID, syncOwner, syncName)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	switch event {
	case "push":
		for _, p := range matched {
			req, ok := a.pushOpen(r.Context(), p.Snapshot(), body)
			if !ok {
				continue
			}
			if _, err := a.open.Open(r.Context(), req); err != nil {
				if errors.Is(err, runapp.ErrDuplicate) {
					continue
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
	case "pull_request":
		for _, p := range matched {
			req, ok := a.pullRequestOpen(r.Context(), p.Snapshot(), body)
			if !ok {
				continue
			}
			if _, err := a.open.Open(r.Context(), req); err != nil {
				if errors.Is(err, runapp.ErrDuplicate) {
					continue
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
	case "issue_comment":
		if err := a.handleIssueComment(r.Context(), matched, body); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func parseRFC3339Ptr(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		utc := t.UTC()
		return &utc
	}
	return nil
}

func (a *api) matchesProjectPaths(ctx context.Context, ps project.Snapshot, changedFiles []string) bool {
	if len(changedFiles) == 0 {
		return true
	}
	tags := ps.Tags
	if a.folders != nil {
		if eff, err := a.folders.Effective(ctx, ps); err == nil {
			tags = eff.EffectiveTags
		}
	}
	var prefixes []string
	if wd := strings.Trim(strings.TrimSpace(ps.WorkingDirectory), "/"); wd != "" && wd != "." {
		prefixes = append(prefixes, wd)
	}
	if watchTag := strings.Trim(strings.TrimSpace(tags["watch"]), "/"); watchTag != "" {
		prefixes = append(prefixes, watchTag)
	}
	if len(prefixes) == 0 {
		return true
	}
	for _, rawFile := range changedFiles {
		f := strings.Trim(strings.TrimSpace(rawFile), "/")
		if f == "" {
			continue
		}
		for _, pref := range prefixes {
			if f == pref || strings.HasPrefix(f, pref+"/") {
				return true
			}
			if matched, err := path.Match(pref, f); err == nil && matched {
				return true
			}
		}
	}
	return false
}

func (a *api) pullRequestOpen(ctx context.Context, ps project.Snapshot, body []byte) (runapp.OpenRequest, bool) {
	var ev struct {
		Action      string   `json:"action"`
		Files       []string `json:"files"`
		PullRequest struct {
			Number            int      `json:"number"`
			Title             string   `json:"title"`
			Draft             bool     `json:"draft"`
			CreatedAt         string   `json:"created_at"`
			ChangedFilesPaths []string `json:"changed_files_paths"`
			Head              struct {
				SHA  string `json:"sha"`
				Ref  string `json:"ref"`
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if json.Unmarshal(body, &ev) != nil {
		return runapp.OpenRequest{}, false
	}
	if head := ev.PullRequest.Head.Repo; head != nil && head.FullName != "" && !strings.EqualFold(head.FullName, ev.Repository.FullName) {
		return runapp.OpenRequest{}, false
	}
	switch ev.Action {
	case "opened", "synchronize", "reopened", "ready_for_review":
	default:
		return runapp.OpenRequest{}, false
	}
	if ev.PullRequest.Draft {
		return runapp.OpenRequest{}, false
	}
	var changedFiles []string
	changedFiles = append(changedFiles, ev.Files...)
	changedFiles = append(changedFiles, ev.PullRequest.ChangedFilesPaths...)
	if !a.matchesProjectPaths(ctx, ps, changedFiles) {
		return runapp.OpenRequest{}, false
	}
	n := ev.PullRequest.Number
	return runapp.OpenRequest{
		ProjectID:   ps.ID,
		Trigger:     run.TriggerPullRequest,
		SHA:         ev.PullRequest.Head.SHA,
		Branch:      ev.PullRequest.Head.Ref,
		TriggeredBy: ev.Sender.Login,
		PRTitle:     ev.PullRequest.Title,
		PRNumber:    &n,
		CommittedAt: parseRFC3339Ptr(ev.PullRequest.CreatedAt),
		PlanOnly:    true,
	}, true
}

func (a *api) pushOpen(ctx context.Context, ps project.Snapshot, body []byte) (runapp.OpenRequest, bool) {
	type commitFiles struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	}
	var ev struct {
		Ref        string        `json:"ref"`
		After      string        `json:"after"`
		Commits    []commitFiles `json:"commits"`
		HeadCommit struct {
			Message   string   `json:"message"`
			Timestamp string   `json:"timestamp"`
			Added     []string `json:"added"`
			Modified  []string `json:"modified"`
			Removed   []string `json:"removed"`
			Author    struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"head_commit"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if json.Unmarshal(body, &ev) != nil {
		return runapp.OpenRequest{}, false
	}
	if ev.After == strings.Repeat("0", 40) {
		return runapp.OpenRequest{}, false
	}
	const heads = "refs/heads/"
	if ev.Ref != heads+ps.DefaultBranch {
		return runapp.OpenRequest{}, false
	}
	var changedFiles []string
	for _, c := range ev.Commits {
		changedFiles = append(changedFiles, c.Added...)
		changedFiles = append(changedFiles, c.Modified...)
		changedFiles = append(changedFiles, c.Removed...)
	}
	changedFiles = append(changedFiles, ev.HeadCommit.Added...)
	changedFiles = append(changedFiles, ev.HeadCommit.Modified...)
	changedFiles = append(changedFiles, ev.HeadCommit.Removed...)
	if !a.matchesProjectPaths(ctx, ps, changedFiles) {
		return runapp.OpenRequest{}, false
	}
	return runapp.OpenRequest{
		ProjectID:   ps.ID,
		Trigger:     run.TriggerPush,
		SHA:         ev.After,
		Branch:      strings.TrimPrefix(ev.Ref, heads),
		Message:     ev.HeadCommit.Message,
		Author:      ev.HeadCommit.Author.Name,
		TriggeredBy: ev.Sender.Login,
		CommittedAt: parseRFC3339Ptr(ev.HeadCommit.Timestamp),
		PlanOnly:    false,
	}, true
}

func parseChatOpsCommand(body string) (cmd string, targetProject string, ok bool) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		if !strings.EqualFold(fields[0], "/infra-guy") {
			continue
		}
		switch strings.ToLower(fields[1]) {
		case "plan":
			cmd = "plan"
		case "approve":
			cmd = "approve"
		case "apply", "confirm":
			cmd = "apply"
		default:
			continue
		}
		if len(fields) >= 3 {
			targetProject = fields[2]
		}
		return cmd, targetProject, true
	}
	return "", "", false
}

func (a *api) handleIssueComment(ctx context.Context, matched []*project.Project, body []byte) error {
	var ev struct {
		Action string `json:"action"`
		Issue  struct {
			Number      int    `json:"number"`
			Title       string `json:"title"`
			PullRequest *struct {
				URL string `json:"url"`
			} `json:"pull_request"`
		} `json:"issue"`
		Comment struct {
			Body string `json:"body"`
		} `json:"comment"`
		Sender struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"sender"`
	}
	if json.Unmarshal(body, &ev) != nil {
		return nil
	}
	if ev.Action != "created" || ev.Issue.PullRequest == nil || ev.Issue.Number <= 0 {
		return nil
	}
	cmd, targetProject, ok := parseChatOpsCommand(ev.Comment.Body)
	if !ok {
		return nil
	}
	prNum := ev.Issue.Number
	runner, hasRunner := a.open.(chatOpsRunner)

	for _, p := range matched {
		ps := p.Snapshot()
		if targetProject != "" && !strings.EqualFold(ps.Name, targetProject) {
			continue
		}

		var (
			latestPRRun         *run.Snapshot
			latestNeedsApproval *run.Snapshot
			latestApprovable    *run.Snapshot
		)
		if hasRunner {
			runs, err := runner.ListByProject(ctx, ps.ID)
			if err != nil {
				return err
			}
			for _, rn := range runs {
				rs := rn.Snapshot()
				matchesPR := rs.PRNumber != nil && *rs.PRNumber == prNum
				if matchesPR && latestPRRun == nil {
					cp := rs
					latestPRRun = &cp
				}
				if rs.Status == run.StatusNeedsApproval {
					if matchesPR || latestNeedsApproval == nil {
						cp := rs
						latestNeedsApproval = &cp
					}
				}
				if rs.Status == run.StatusNeedsApproval || (rs.PlanOnly && rs.Status == run.StatusPlanned) {
					if matchesPR || latestApprovable == nil {
						cp := rs
						latestApprovable = &cp
					}
				}
			}
		}

		switch cmd {
		case "plan":
			if latestPRRun == nil || latestPRRun.SHA == "" {
				continue
			}
			title := latestPRRun.PRTitle
			if title == "" {
				title = ev.Issue.Title
			}
			_, err := a.open.Open(ctx, runapp.OpenRequest{
				ProjectID:   ps.ID,
				Trigger:     run.TriggerPullRequest,
				SHA:         latestPRRun.SHA,
				Branch:      latestPRRun.Branch,
				TriggeredBy: ev.Sender.Login,
				PRTitle:     title,
				PRNumber:    &prNum,
				CommittedAt: latestPRRun.CommittedAt,
				PlanOnly:    true,
			})
			if err != nil && !errors.Is(err, runapp.ErrDuplicate) {
				return err
			}
		case "approve":
			if !hasRunner || latestApprovable == nil {
				continue
			}
			var senderID string
			if ev.Sender.ID > 0 {
				senderID = strconv.FormatInt(ev.Sender.ID, 10)
			}
			err := runner.Approve(ctx, latestApprovable.ID, run.Approval{
				UserID: senderID,
				Login:  ev.Sender.Login,
				Role:   "maintainer",
			})
			if err != nil && !errors.Is(err, run.ErrNotUnconfirmed) {
				return err
			}
		case "apply":
			if hasRunner && latestNeedsApproval != nil {
				var senderID string
				if ev.Sender.ID > 0 {
					senderID = strconv.FormatInt(ev.Sender.ID, 10)
				}
				_ = runner.Approve(ctx, latestNeedsApproval.ID, run.Approval{
					UserID: senderID,
					Login:  ev.Sender.Login,
					Role:   "maintainer",
				})
				err := runner.Confirm(ctx, latestNeedsApproval.ID, ev.Sender.Login)
				if err != nil && !errors.Is(err, run.ErrNotUnconfirmed) {
					return err
				}
				continue
			}
			if latestPRRun != nil && latestPRRun.SHA != "" {
				title := latestPRRun.PRTitle
				if title == "" {
					title = ev.Issue.Title
				}
				_, err := a.open.Open(ctx, runapp.OpenRequest{
					ProjectID:   ps.ID,
					Trigger:     run.TriggerPullRequest,
					SHA:         latestPRRun.SHA,
					Branch:      latestPRRun.Branch,
					TriggeredBy: ev.Sender.Login,
					PRTitle:     title,
					PRNumber:    &prNum,
					CommittedAt: latestPRRun.CommittedAt,
					PlanOnly:    false,
				})
				if err != nil && !errors.Is(err, runapp.ErrDuplicate) {
					return err
				}
			}
		}
	}
	return nil
}
