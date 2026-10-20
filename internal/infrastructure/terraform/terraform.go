package terraform

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"infra-guy/internal/domain/run"
)

const WaitDelay = 15 * time.Second

type Executor struct{}

type Request struct {
	WorkDir, InternalBaseURL, ProjectID, RunID, StateToken, WorkingDirectory, GitHubToken string
	Step                                                                                  run.StepName
	PlanFile                                                                              []byte
}

type Result struct {
	ExitCode                int
	Logs, PlanText, Version string
	PlanFile                []byte
}

var initArgs = []string{"init", "-input=false", "-no-color"}

func commands(step run.StepName) [][]string {
	switch step {
	case run.StepInit:
		return [][]string{initArgs}
	case run.StepPlan:
		return [][]string{
			initArgs,
			{"plan", "-input=false", "-no-color", "-out=tfplan"},
			{"show", "-no-color", "tfplan"},
		}
	case run.StepApply:
		return [][]string{initArgs, {"apply", "-input=false", "-no-color", "tfplan"}}
	default:
		return nil
	}
}

func (e Executor) Run(ctx context.Context, req Request) (Result, error) {
	if req.Step == run.StepApply && len(req.PlanFile) == 0 {
		return Result{ExitCode: 1, Logs: "apply requires a stored plan file"}, nil
	}

	dir := req.WorkDir
	if req.WorkingDirectory != "" {
		dir = filepath.Join(req.WorkDir, req.WorkingDirectory)
	}

	backend := backendFile(req.InternalBaseURL, req.ProjectID, req.RunID, req.StateToken)
	if err := os.WriteFile(filepath.Join(dir, "_platform_backend.tf.json"), backend, 0o644); err != nil {
		return Result{}, err
	}

	if hasBackend(dir) {
		return Result{ExitCode: 1, Logs: "the platform owns Terraform state; remove the backend block from this repository"}, nil
	}

	cmds := commands(req.Step)
	if cmds == nil {
		return Result{ExitCode: 1, Logs: "unknown terraform step"}, nil
	}
	if req.Step == run.StepApply {
		if err := os.WriteFile(filepath.Join(dir, "tfplan"), req.PlanFile, 0o644); err != nil {
			return Result{}, err
		}
	}

	env := commandEnv(req.GitHubToken)
	var logs strings.Builder
	var planText string
	for _, args := range cmds {
		out, err := command(ctx, dir, env, args...).CombinedOutput()
		logs.Write(out)
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return Result{ExitCode: ee.ExitCode(), Logs: logs.String()}, nil
			}
			return Result{}, err
		}
		if args[0] == "show" {
			planText = string(out)
		}
	}

	res := Result{ExitCode: 0, Logs: logs.String(), PlanText: planText}
	if req.Step == run.StepPlan {
		plan, err := os.ReadFile(filepath.Join(dir, "tfplan"))
		if err != nil {
			return Result{}, err
		}
		res.PlanFile = plan
	}
	if req.Step == run.StepInit || req.Step == run.StepPlan {
		res.Version = terraformVersion(ctx, dir, env)
	}
	return res, nil
}

func command(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "terraform", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = WaitDelay
	return cmd
}

var blockedEnv = map[string]bool{
	"DATABASE_URL":        true,
	"BASIC_AUTH_USER":     true,
	"BASIC_AUTH_PASSWORD": true,
	"TF_LOG":              true,
	"GIT_TRACE":           true,
}

func commandEnv(githubToken string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if blockedEnv[k] || (githubToken != "" && v == githubToken) {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TF_IN_AUTOMATION=1", "TF_INPUT=0")
}

func backendFile(baseURL, projectID, runID, token string) []byte {
	state := strings.TrimRight(baseURL, "/") + "/api/projects/" + projectID + "/state"
	address := state
	if runID != "" {
		address += "?" + url.Values{"run": {runID}}.Encode()
	}
	lock := state + "/lock"
	doc := map[string]any{
		"terraform": []any{
			map[string]any{
				"backend": []any{
					map[string]any{
						"http": map[string]any{
							"address":        address,
							"lock_address":   lock,
							"unlock_address": lock,
							"lock_method":    "LOCK",
							"unlock_method":  "UNLOCK",
							"username":       "terraform",
							"password":       token,
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return []byte("{}")
	}
	return b
}

var backendRe = regexp.MustCompile(`(?m)^\s*backend\s+"`)

func hasBackend(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if backendRe.Match(b) {
			return true
		}
	}
	return false
}

func terraformVersion(ctx context.Context, dir string, env []string) string {
	out, err := command(ctx, dir, env, "version", "-json").CombinedOutput()
	if err != nil {
		return ""
	}
	var v struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return ""
	}
	return v.Version
}
