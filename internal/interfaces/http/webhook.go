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
	"strings"

	runapp "infra-guy/internal/application/run"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
)

type opener interface {
	Open(context.Context, runapp.OpenRequest) (*run.Run, error)
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
	id := project.ID(r.PathValue("id"))
	p, err := a.projects.Get(r.Context(), id)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if p == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !validSignature(p.Snapshot().WebhookSecret, r.Header.Get("X-Hub-Signature-256"), body) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		w.WriteHeader(http.StatusOK)
		return
	}

	var req runapp.OpenRequest
	var ok bool
	switch event {
	case "pull_request":
		req, ok = pullRequestOpen(id, body)
	case "push":
		req, ok = pushOpen(id, p.Snapshot().DefaultBranch, body)
	}
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := a.open.Open(r.Context(), req); err != nil {
		if errors.Is(err, runapp.ErrDuplicate) {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func pullRequestOpen(id project.ID, body []byte) (runapp.OpenRequest, bool) {
	var ev struct {
		Action      string `json:"action"`
		PullRequest struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Draft  bool   `json:"draft"`
			Head   struct {
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
	n := ev.PullRequest.Number
	return runapp.OpenRequest{
		ProjectID:   id,
		Trigger:     run.TriggerPullRequest,
		SHA:         ev.PullRequest.Head.SHA,
		Branch:      ev.PullRequest.Head.Ref,
		TriggeredBy: ev.Sender.Login,
		PRTitle:     ev.PullRequest.Title,
		PRNumber:    &n,
		PlanOnly:    true,
	}, true
}

func pushOpen(id project.ID, defaultBranch string, body []byte) (runapp.OpenRequest, bool) {
	var ev struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		HeadCommit struct {
			Message string `json:"message"`
			Author  struct {
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
	if ev.Ref != heads+defaultBranch {
		return runapp.OpenRequest{}, false
	}
	return runapp.OpenRequest{
		ProjectID:   id,
		Trigger:     run.TriggerPush,
		SHA:         ev.After,
		Branch:      strings.TrimPrefix(ev.Ref, heads),
		Message:     ev.HeadCommit.Message,
		Author:      ev.HeadCommit.Author.Name,
		TriggeredBy: ev.Sender.Login,
		PlanOnly:    false,
	}, true
}
