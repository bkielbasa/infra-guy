package terraform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/tfcontext"
)

const WaitDelay = 15 * time.Second

type Executor struct {
	Binary BinaryLocator
}

type Request struct {
	WorkDir, InternalBaseURL, ProjectID, RunID, StateToken, WorkingDirectory, GitHubToken string
	TerraformVersion                                                                      string
	Step                                                                                  run.StepName
	PlanFile                                                                              []byte
	Env                                                                                   map[string]string
	Files                                                                                 map[string][]byte
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
	// A later *_override.tf.json replaces any backend or cloud block in the repository.
	if err := os.WriteFile(filepath.Join(dir, "~infraguy_backend_override.tf.json"), backend, 0o644); err != nil {
		return Result{}, err
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

	bin := "terraform"
	if e.Binary != nil && req.TerraformVersion != "" {
		var err error
		bin, err = e.Binary.Ensure(ctx, req.TerraformVersion)
		if err != nil {
			return Result{}, err
		}
	}

	homeDir := filepath.Join(req.WorkDir, ".home")
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return Result{}, err
	}
	if err := materializeFiles(homeDir, req.Files); err != nil {
		return Result{}, err
	}

	env := commandEnv(req.GitHubToken, homeDir, req.Env)
	var logs strings.Builder
	var planText string
	for _, args := range cmds {
		out, err := commandBin(ctx, bin, dir, env, args...).CombinedOutput()
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
		res.Version = terraformVersionBin(ctx, bin, dir, env)
	}
	return res, nil
}

func materializeFiles(homeDir string, files map[string][]byte) error {
	cleanHome := filepath.Clean(homeDir)
	for relPath, content := range files {
		cleanRel, err := tfcontext.ValidateFilePath(relPath)
		if err != nil {
			return err
		}
		target := filepath.Join(cleanHome, filepath.FromSlash(cleanRel))
		rel, err := filepath.Rel(cleanHome, target)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid file path %q", relPath)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func command(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	return commandBin(ctx, "terraform", dir, env, args...)
}

func commandBin(ctx context.Context, bin, dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = WaitDelay
	return cmd
}

var blockedEnv = map[string]bool{
	"DATABASE_URL":                true,
	"POSTGRES_PASSWORD":           true,
	"BASIC_AUTH_USER":             true,
	"BASIC_AUTH_PASSWORD":         true,
	"GITHUB_APP_ID":               true,
	"GITHUB_APP_PRIVATE_KEY":      true,
	"GITHUB_APP_PRIVATE_KEY_FILE": true,
	"GITHUB_APP_WEBHOOK_SECRET":   true,
	"GITHUB_APP_CLIENT_ID":        true,
	"GITHUB_APP_CLIENT_SECRET":    true,
	"CONTEXT_ENCRYPTION_KEY":      true,
	"TF_LOG":                      true,
	"GIT_TRACE":                   true,
}

func commandEnv(githubToken, homeDir string, extra map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if blockedEnv[k] || k == "HOME" || (githubToken != "" && v == githubToken) {
			continue
		}
		env = append(env, kv)
	}
	if len(extra) > 0 {
		keys := make([]string, 0, len(extra))
		for k := range extra {
			if blockedEnv[k] || k == "HOME" || k == "PATH" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+extra[k])
		}
	}
	if homeDir != "" {
		env = append(env, "HOME="+homeDir)
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

func terraformVersion(ctx context.Context, dir string, env []string) string {
	return terraformVersionBin(ctx, "terraform", dir, env)
}

func terraformVersionBin(ctx context.Context, bin, dir string, env []string) string {
	out, err := commandBin(ctx, bin, dir, env, "version", "-json").CombinedOutput()
	if err != nil {
		return ""
	}
	return parseVersionOutput(out)
}

func parseVersionOutput(out []byte) string {
	var v struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return ""
	}
	return v.Version
}
