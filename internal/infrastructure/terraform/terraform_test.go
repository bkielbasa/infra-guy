package terraform

import (
	"context"
	"os"
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

func TestHasBackend(t *testing.T) {
	with := t.TempDir()
	if err := os.WriteFile(filepath.Join(with, "main.tf"), []byte("terraform {\n  backend \"s3\" {\n    bucket = \"x\"\n  }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasBackend(with) {
		t.Fatal("expected backend block")
	}

	comment := t.TempDir()
	if err := os.WriteFile(filepath.Join(comment, "main.tf"), []byte("# backend \"s3\" is commented\nresource \"null_resource\" \"x\" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasBackend(comment) {
		t.Fatal("comment should not count as a backend block")
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

func TestRunRejectsDeclaredBackend(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "infra")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "main.tf"), []byte("terraform {\n  backend \"s3\" {}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var e Executor
	res, err := e.Run(context.Background(), Request{
		WorkDir:          dir,
		WorkingDirectory: "infra",
		InternalBaseURL:  "http://127.0.0.1",
		ProjectID:        "proj-1",
		StateToken:       "tok",
		Step:             run.StepInit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 1 {
		t.Fatalf("exit %d", res.ExitCode)
	}
	const want = "the platform owns Terraform state; remove the backend block from this repository"
	if !strings.Contains(res.Logs, want) {
		t.Fatalf("logs: %q", res.Logs)
	}
	if _, err := os.Stat(filepath.Join(nested, "_platform_backend.tf.json")); err != nil {
		t.Fatalf("backend file: %v", err)
	}
}
