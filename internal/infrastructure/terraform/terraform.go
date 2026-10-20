package terraform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"infra-guy/internal/domain/run"
)

type Executor struct{}

type Request struct {
	WorkDir, InternalBaseURL, ProjectID, StateToken, WorkingDirectory string
	Step                                                              run.StepName
	PlanFile                                                          []byte
}

type Result struct {
	ExitCode                int
	Logs, PlanText, Version string
	PlanFile                []byte
}

func (e Executor) Run(ctx context.Context, req Request) (Result, error) {
	if req.Step == run.StepApply && len(req.PlanFile) == 0 {
		return Result{ExitCode: 1, Logs: "apply requires a stored plan file"}, nil
	}

	dir := req.WorkDir
	if req.WorkingDirectory != "" {
		dir = filepath.Join(req.WorkDir, req.WorkingDirectory)
	}

	backend := backendFile(req.InternalBaseURL, req.ProjectID, req.StateToken)
	if err := os.WriteFile(filepath.Join(dir, "_platform_backend.tf.json"), backend, 0o644); err != nil {
		return Result{}, err
	}

	if hasBackend(dir) {
		return Result{ExitCode: 1, Logs: "the platform owns Terraform state; remove the backend block from this repository"}, nil
	}

	var cmds [][]string
	switch req.Step {
	case run.StepInit:
		cmds = [][]string{{"init", "-input=false", "-no-color"}}
	case run.StepPlan:
		cmds = [][]string{
			{"plan", "-input=false", "-no-color", "-out=tfplan"},
			{"show", "-no-color", "tfplan"},
		}
	case run.StepApply:
		if err := os.WriteFile(filepath.Join(dir, "tfplan"), req.PlanFile, 0o644); err != nil {
			return Result{}, err
		}
		cmds = [][]string{{"apply", "-input=false", "-no-color", "tfplan"}}
	default:
		return Result{ExitCode: 1, Logs: "unknown terraform step"}, nil
	}

	var logs strings.Builder
	var planText string
	for i, args := range cmds {
		cmd := exec.CommandContext(ctx, "terraform", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0")
		out, err := cmd.CombinedOutput()
		logs.Write(out)
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return Result{ExitCode: ee.ExitCode(), Logs: logs.String()}, nil
			}
			return Result{}, err
		}
		if req.Step == run.StepPlan && i == 1 {
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
		res.Version = terraformVersion(ctx, dir)
	}
	return res, nil
}

func backendFile(baseURL, projectID, token string) []byte {
	address := strings.TrimRight(baseURL, "/") + "/api/projects/" + projectID + "/state"
	lock := address + "/lock"
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

func terraformVersion(ctx context.Context, dir string) string {
	cmd := exec.CommandContext(ctx, "terraform", "version", "-json")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0")
	out, err := cmd.CombinedOutput()
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
