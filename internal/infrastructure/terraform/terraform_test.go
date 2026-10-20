package terraform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra-guy/internal/domain/run"
)

func TestBackendFile(t *testing.T) {
	raw := string(backendFile("http://127.0.0.1:8080", "proj-1", "state-tok"))
	if !strings.Contains(raw, `"address"`) {
		t.Fatalf("missing address key: %s", raw)
	}
	if !strings.Contains(raw, "http://127.0.0.1:8080/api/projects/proj-1/state") {
		t.Fatalf("address: %s", raw)
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
