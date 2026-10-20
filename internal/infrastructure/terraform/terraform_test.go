package terraform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"infra-guy/internal/domain/run"
)

func TestBackendFile(t *testing.T) {
	raw := string(backendFile("http://127.0.0.1:8080", "proj-1", "run-7", "state-tok"))
	if !strings.Contains(raw, `"address"`) {
		t.Fatalf("missing address key: %s", raw)
	}
	if !strings.Contains(raw, `"address":"http://127.0.0.1:8080/api/projects/proj-1/state?run=run-7"`) {
		t.Fatalf("address: %s", raw)
	}
	if !strings.Contains(raw, `"lock_address":"http://127.0.0.1:8080/api/projects/proj-1/state/lock"`) {
		t.Fatalf("lock address: %s", raw)
	}
	if !strings.Contains(raw, "terraform") {
		t.Fatalf("missing username terraform: %s", raw)
	}
	if !strings.Contains(raw, "state-tok") {
		t.Fatalf("missing token: %s", raw)
	}
}

func TestPlanAndApplyStartWithInit(t *testing.T) {
	for _, step := range []run.StepName{run.StepPlan, run.StepApply} {
		cmds := commands(step)
		if len(cmds) < 2 || strings.Join(cmds[0], " ") != "init -input=false -no-color" {
			t.Fatalf("%s commands %#v", step, cmds)
		}
	}
	if init := commands(run.StepInit); len(init) != 1 || init[0][0] != "init" {
		t.Fatalf("init commands %#v", init)
	}
	if plan := commands(run.StepPlan); plan[1][0] != "plan" || plan[2][0] != "show" {
		t.Fatalf("plan commands %#v", plan)
	}
	if apply := commands(run.StepApply); apply[1][0] != "apply" {
		t.Fatalf("apply commands %#v", apply)
	}
}

func TestCommandEnvDropsPlatformSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret")
	t.Setenv("BASIC_AUTH_USER", "admin")
	t.Setenv("BASIC_AUTH_PASSWORD", "pw")
	t.Setenv("TF_LOG", "TRACE")
	t.Setenv("GIT_TRACE", "1")
	t.Setenv("SOME_GH_COPY", "ghp_project_token")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	env := strings.Join(commandEnv("ghp_project_token"), "\n")
	for _, banned := range []string{"DATABASE_URL=", "BASIC_AUTH_USER=", "BASIC_AUTH_PASSWORD=", "TF_LOG=", "GIT_TRACE=", "ghp_project_token"} {
		if strings.Contains(env, banned) {
			t.Fatalf("env leaks %q", banned)
		}
	}
	for _, want := range []string{"AWS_SECRET_ACCESS_KEY=aws-secret", "TF_IN_AUTOMATION=1", "TF_INPUT=0"} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q", want)
		}
	}
}

func TestCommandInterruptsBeforeKill(t *testing.T) {
	cmd := command(context.Background(), t.TempDir(), nil, "version")
	if cmd.Cancel == nil || cmd.WaitDelay != 15*time.Second {
		t.Fatalf("cancel set %v wait delay %s", cmd.Cancel != nil, cmd.WaitDelay)
	}
}

func TestRunApplyRequiresPlanFile(t *testing.T) {
	var e Executor
	res, err := e.Run(context.Background(), Request{Step: run.StepApply})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 1 {
		t.Fatalf("exit %d", res.ExitCode)
	}
	if !strings.Contains(res.Logs, "apply requires a stored plan file") {
		t.Fatalf("logs: %q", res.Logs)
	}
}

func TestRunOverridesDeclaredBackend(t *testing.T) {
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Fatal("terraform is required")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cases := []string{
		"terraform {\n  backend \"s3\" {\n    bucket = \"keep-me\"\n  }\n}\n",
		"terraform {\n  cloud {\n    organization = \"keep-me\"\n    workspaces { name = \"prod\" }\n  }\n}\n",
	}
	for _, src := range cases {
		t.Run(src[:20], func(t *testing.T) {
			dir := t.TempDir()
			nested := filepath.Join(dir, "infra")
			if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(nested, "main.tf"), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			competing := "terraform {\n  backend \"local\" {\n    path = \"not-platform.tfstate\"\n  }\n}\n"
			if err := os.WriteFile(filepath.Join(nested, "zzz_backend_override.tf"), []byte(competing), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var e Executor
			res, err := e.Run(ctx, Request{
				WorkDir:          dir,
				WorkingDirectory: "infra",
				InternalBaseURL:  srv.URL,
				ProjectID:        "proj-1",
				RunID:            "run-7",
				StateToken:       "state-tok",
				Step:             run.StepInit,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != 0 {
				t.Fatalf("exit %d\n%s", res.ExitCode, res.Logs)
			}
			written, err := os.ReadFile(filepath.Join(nested, "main.tf"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(written), "keep-me") {
				t.Fatal("repository backend was rewritten")
			}
			state, err := os.ReadFile(filepath.Join(nested, ".terraform", "terraform.tfstate"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(state), srv.URL+"/api/projects/proj-1/state") {
				t.Fatalf("platform backend not in use: %s", state)
			}
			if strings.Contains(string(state), "keep-me") || strings.Contains(string(state), "not-platform") {
				t.Fatalf("declared backend still active: %s", state)
			}
		})
	}
}
