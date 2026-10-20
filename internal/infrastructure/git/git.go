package git

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"infra-guy/internal/domain/project"
)

type Checkout struct{}

func (c Checkout) Clone(ctx context.Context, dest, cloneURL, token, sha string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if err := execGit(ctx, dest, token, "init"); err != nil {
		return err
	}
	if err := execGit(ctx, dest, "", "remote", "add", "origin", cloneURL); err != nil {
		return err
	}
	if err := execGit(ctx, dest, token, "fetch", "--depth", "1", "origin", sha); err != nil {
		return err
	}
	return execGit(ctx, dest, "", "checkout", "--detach", "FETCH_HEAD")
}

func (c Checkout) BranchTip(ctx context.Context, cloneURL, token, branch string) (string, error) {
	out, err := gitOutput(ctx, "", token, "ls-remote", cloneURL, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("git ls-remote: no output")
	}
	return fields[0], nil
}

type CommitLookup struct{}

func (l CommitLookup) Lookup(ctx context.Context, repo project.SourceRepo, token, sha string) (message, author string, err error) {
	u := "https://api.github.com/repos/" + repo.Owner + "/" + repo.Name + "/commits/" + sha
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", nil
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", nil
	}
	var body struct {
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", nil
	}
	return body.Commit.Message, body.Commit.Author.Name, nil
}

func cloneArgs(url, token, sha string) []string {
	return append(authArgs(token), "fetch", "--depth", "1", url, sha)
}

func authArgs(token string) []string {
	if token == "" {
		return nil
	}
	return []string{"-c", "http.extraheader=AUTHORIZATION: bearer " + token}
}

func execGit(ctx context.Context, dir, token string, args ...string) error {
	_, err := gitOutput(ctx, dir, token, args...)
	return err
}

func gitOutput(ctx context.Context, dir, token string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(authArgs(token), args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}
