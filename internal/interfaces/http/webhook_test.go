package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	runapp "infra-guy/internal/application/run"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
)

type fakeOpen struct {
	reqs []runapp.OpenRequest
	err  error
}

func (f *fakeOpen) Open(_ context.Context, in runapp.OpenRequest) (*run.Run, error) {
	f.reqs = append(f.reqs, in)
	return nil, f.err
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func webhookMux(t *testing.T, projects *memProjects, open *fakeOpen) http.Handler {
	t.Helper()
	return New(nil, open, projects)
}

func postWebhook(t *testing.T, h http.Handler, id, event, secret string, body []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/"+id, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	} else if secret != "" {
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestGitHubWebhook(t *testing.T) {
	p := testProject(t)
	if err := p.SetDefaultBranch("trunk"); err != nil {
		t.Fatal(err)
	}
	projects := &memProjects{items: map[project.ID]*project.Project{"p1": p}}
	open := &fakeOpen{}
	h := webhookMux(t, projects, open)

	body := []byte(`{"zen":"ok"}`)
	rr := postWebhook(t, h, "missing", "ping", "whsec", body, sign("whsec", body))
	if rr.Code != 404 || len(open.reqs) != 0 {
		t.Fatalf("missing project %d calls %d", rr.Code, len(open.reqs))
	}

	rr = postWebhook(t, h, "p1", "ping", "", body, "")
	if rr.Code != 401 || len(open.reqs) != 0 {
		t.Fatalf("missing sig %d calls %d", rr.Code, len(open.reqs))
	}

	rr = postWebhook(t, h, "p1", "ping", "", body, sign("wrong", body))
	if rr.Code != 401 || len(open.reqs) != 0 {
		t.Fatalf("bad sig %d calls %d", rr.Code, len(open.reqs))
	}

	rr = postWebhook(t, h, "p1", "ping", "whsec", body, "")
	if rr.Code != 200 || len(open.reqs) != 0 {
		t.Fatalf("ping %d calls %d", rr.Code, len(open.reqs))
	}

	prOpened := []byte(`{
		"action":"opened",
		"pull_request":{"number":12,"title":"add lock","draft":false,"head":{"sha":"abc123","ref":"feat"}},
		"sender":{"login":"alice"}
	}`)
	rr = postWebhook(t, h, "p1", "pull_request", "whsec", prOpened, "")
	if rr.Code != 200 || len(open.reqs) != 1 {
		t.Fatalf("pr opened %d calls %d", rr.Code, len(open.reqs))
	}
	got := open.reqs[0]
	if got.ProjectID != "p1" || got.Trigger != run.TriggerPullRequest || got.SHA != "abc123" || got.Branch != "feat" || got.PRTitle != "add lock" || got.TriggeredBy != "alice" || !got.PlanOnly {
		t.Fatalf("pr req %#v", got)
	}
	if got.PRNumber == nil || *got.PRNumber != 12 {
		t.Fatalf("pr number %#v", got.PRNumber)
	}

	for _, action := range []string{"synchronize", "reopened", "ready_for_review"} {
		open.reqs = nil
		payload := []byte(`{"action":"` + action + `","pull_request":{"number":3,"title":"t","draft":false,"head":{"sha":"s","ref":"b"}},"sender":{"login":"bob"}}`)
		rr = postWebhook(t, h, "p1", "pull_request", "whsec", payload, "")
		if rr.Code != 200 || len(open.reqs) != 1 || !open.reqs[0].PlanOnly || open.reqs[0].Trigger != run.TriggerPullRequest {
			t.Fatalf("action %s %d %#v", action, rr.Code, open.reqs)
		}
	}

	open.reqs = nil
	fork := []byte(`{"action":"opened","pull_request":{"number":4,"title":"f","draft":false,"head":{"sha":"s","ref":"b","repo":{"full_name":"mallory/infra"}}},"repository":{"full_name":"bklebasa/infra"},"sender":{"login":"mallory"}}`)
	rr = postWebhook(t, h, "p1", "pull_request", "whsec", fork, "")
	if rr.Code != 200 || len(open.reqs) != 0 {
		t.Fatalf("fork %d calls %d", rr.Code, len(open.reqs))
	}
	sameRepo := []byte(`{"action":"opened","pull_request":{"number":5,"title":"s","draft":false,"head":{"sha":"s","ref":"b","repo":{"full_name":"bklebasa/infra"}}},"repository":{"full_name":"bklebasa/infra"},"sender":{"login":"alice"}}`)
	rr = postWebhook(t, h, "p1", "pull_request", "whsec", sameRepo, "")
	if rr.Code != 200 || len(open.reqs) != 1 {
		t.Fatalf("same repo %d calls %d", rr.Code, len(open.reqs))
	}

	open.reqs = nil
	draft := []byte(`{"action":"opened","pull_request":{"number":1,"title":"d","draft":true,"head":{"sha":"s","ref":"b"}},"sender":{"login":"alice"}}`)
	rr = postWebhook(t, h, "p1", "pull_request", "whsec", draft, "")
	if rr.Code != 200 || len(open.reqs) != 0 {
		t.Fatalf("draft %d calls %d", rr.Code, len(open.reqs))
	}

	closed := []byte(`{"action":"closed","pull_request":{"number":1,"title":"d","draft":false,"head":{"sha":"s","ref":"b"}},"sender":{"login":"alice"}}`)
	rr = postWebhook(t, h, "p1", "pull_request", "whsec", closed, "")
	if rr.Code != 200 || len(open.reqs) != 0 {
		t.Fatalf("closed %d calls %d", rr.Code, len(open.reqs))
	}

	push := []byte(`{
		"ref":"refs/heads/trunk",
		"after":"ffffffffffffffffffffffffffffffffffffffff",
		"head_commit":{"message":"ship it","author":{"name":"Cara"}},
		"sender":{"login":"cara"}
	}`)
	rr = postWebhook(t, h, "p1", "push", "whsec", push, "")
	if rr.Code != 200 || len(open.reqs) != 1 {
		t.Fatalf("push %d calls %d", rr.Code, len(open.reqs))
	}
	got = open.reqs[0]
	if got.Trigger != run.TriggerPush || got.PlanOnly || got.SHA != "ffffffffffffffffffffffffffffffffffffffff" || got.Branch != "trunk" || got.Message != "ship it" || got.Author != "Cara" || got.TriggeredBy != "cara" || got.ProjectID != "p1" {
		t.Fatalf("push req %#v", got)
	}

	open.reqs = nil
	other := []byte(`{"ref":"refs/heads/other","after":"ffffffffffffffffffffffffffffffffffffffff","head_commit":{"message":"x","author":{"name":"Cara"}},"sender":{"login":"cara"}}`)
	rr = postWebhook(t, h, "p1", "push", "whsec", other, "")
	if rr.Code != 200 || len(open.reqs) != 0 {
		t.Fatalf("other branch %d calls %d", rr.Code, len(open.reqs))
	}

	zeros := []byte(`{"ref":"refs/heads/trunk","after":"0000000000000000000000000000000000000000","head_commit":{"message":"x","author":{"name":"Cara"}},"sender":{"login":"cara"}}`)
	rr = postWebhook(t, h, "p1", "push", "whsec", zeros, "")
	if rr.Code != 200 || len(open.reqs) != 0 {
		t.Fatalf("deleted %d calls %d", rr.Code, len(open.reqs))
	}

	open.err = runapp.ErrDuplicate
	open.reqs = nil
	rr = postWebhook(t, h, "p1", "push", "whsec", push, "")
	if rr.Code != 200 || len(open.reqs) != 1 {
		t.Fatalf("duplicate %d calls %d", rr.Code, len(open.reqs))
	}

	open.err = errors.New("boom")
	open.reqs = nil
	rr = postWebhook(t, h, "p1", "push", "whsec", push, "")
	if rr.Code != 500 || len(open.reqs) != 1 {
		t.Fatalf("open error %d calls %d", rr.Code, len(open.reqs))
	}
}
