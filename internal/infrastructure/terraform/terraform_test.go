package terraform

import (
	"archive/zip"
	"bytes"
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
	if plan := commands(run.StepPlan); len(plan) != 4 || plan[1][0] != "plan" || plan[2][0] != "show" || plan[3][0] != "show" || plan[3][1] != "-json" {
		t.Fatalf("plan commands %#v", plan)
	}
	if apply := commands(run.StepApply); apply[1][0] != "apply" {
		t.Fatalf("apply commands %#v", apply)
	}
}

func TestCommandEnvDropsPlatformSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret")
	t.Setenv("POSTGRES_PASSWORD", "db-pw")
	t.Setenv("BASIC_AUTH_USER", "admin")
	t.Setenv("BASIC_AUTH_PASSWORD", "pw")
	t.Setenv("GITHUB_APP_ID", "42")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "pem-bytes")
	t.Setenv("GITHUB_APP_PRIVATE_KEY_FILE", "/run/secrets/key.pem")
	t.Setenv("GITHUB_APP_WEBHOOK_SECRET", "wh-secret")
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.abc")
	t.Setenv("GITHUB_APP_CLIENT_SECRET", "client-secret")
	t.Setenv("CONTEXT_ENCRYPTION_KEY", "enc-secret")
	t.Setenv("TF_LOG", "TRACE")
	t.Setenv("GIT_TRACE", "1")
	t.Setenv("SOME_GH_COPY", "ghp_project_token")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	env := strings.Join(commandEnv("ghp_project_token", "/tmp/isolated-home", nil), "\n")
	for _, banned := range []string{
		"DATABASE_URL=",
		"POSTGRES_PASSWORD=",
		"BASIC_AUTH_USER=",
		"BASIC_AUTH_PASSWORD=",
		"GITHUB_APP_ID=",
		"GITHUB_APP_PRIVATE_KEY=",
		"GITHUB_APP_PRIVATE_KEY_FILE=",
		"GITHUB_APP_WEBHOOK_SECRET=",
		"GITHUB_APP_CLIENT_ID=",
		"GITHUB_APP_CLIENT_SECRET=",
		"CONTEXT_ENCRYPTION_KEY=",
		"TF_LOG=",
		"GIT_TRACE=",
		"ghp_project_token",
	} {
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

type staticBinary string

func (s staticBinary) Ensure(context.Context, string) (string, error) {
	return string(s), nil
}

func TestVersionCatalogParsesAndFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"versions":{"0.15.0":{},"1.9.8":{},"1.10.5":{},"1.11.0-beta1":{},"1.8.5":{}}}`))
	}))
	t.Cleanup(srv.Close)

	cat := &VersionCatalog{URL: srv.URL, Client: srv.Client(), TTL: time.Hour}
	got := cat.List(context.Background())
	if len(got) != 3 || got[0] != "1.10.5" || got[1] != "1.9.8" || got[2] != "1.8.5" {
		t.Fatalf("catalog got %v", got)
	}

	offline := &VersionCatalog{URL: "http://127.0.0.1:1"}
	fb := offline.List(context.Background())
	if len(fb) == 0 || fb[0] != FallbackVersions[0] {
		t.Fatalf("fallback got %v", fb)
	}
}

func TestInstallerDownloadsAndCachesBinary(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		fw, err := zw.Create("terraform")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte("#!/bin/sh\necho '{\"terraform_version\":\"1.8.5\"}'\n"))
		_ = zw.Close()
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)

	inst := &Installer{CacheDir: t.TempDir(), BaseURL: srv.URL, Client: srv.Client()}
	bin1, err := inst.Ensure(context.Background(), "1.8.5")
	if err != nil {
		t.Fatal(err)
	}
	bin2, err := inst.Ensure(context.Background(), "1.8.5")
	if err != nil {
		t.Fatal(err)
	}
	if bin1 != bin2 || hits != 1 {
		t.Fatalf("expected cached binary with 1 hit, got hits=%d bin1=%q bin2=%q", hits, bin1, bin2)
	}
}

func TestExecutorMaterializesHomeFilesAndContextEnv(t *testing.T) {
	dir := t.TempDir()
	fakeBin := filepath.Join(t.TempDir(), "fake-terraform")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"version\" ]; then echo '{\"terraform_version\":\"1.10.5\"}'; exit 0; fi\n" +
		"echo \"ENV_FOO=$TF_VAR_foo\"\n" +
		"echo \"HOME_DIR=$HOME\"\n" +
		"cat \"$HOME/.kube/config\"\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	e := Executor{Binary: staticBinary(fakeBin)}
	res, err := e.Run(context.Background(), Request{
		WorkDir:          dir,
		InternalBaseURL:  "http://127.0.0.1:8080",
		ProjectID:        "p1",
		RunID:            "r1",
		StateToken:       "tok",
		Step:             run.StepInit,
		TerraformVersion: "1.10.5",
		Env:              map[string]string{"TF_VAR_foo": "bar-val", "DATABASE_URL": "evil"},
		Files:            map[string][]byte{".kube/config": []byte("kubeconfig-payload\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Logs)
	}
	if !strings.Contains(res.Logs, "ENV_FOO=bar-val") || !strings.Contains(res.Logs, "kubeconfig-payload") {
		t.Fatalf("missing injected env or file in logs: %s", res.Logs)
	}
	if res.Version != "1.10.5" {
		t.Fatalf("version = %q, want 1.10.5", res.Version)
	}
	info, err := os.Stat(filepath.Join(dir, ".home", ".kube", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestExecutorPlanCapturesPlanJSONWithoutPollutingLogs(t *testing.T) {
	dir := t.TempDir()
	fakeBin := filepath.Join(t.TempDir(), "fake-terraform")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"version\" ]; then echo '{\"terraform_version\":\"1.9.8\"}'; exit 0; fi\n" +
		"if [ \"$1\" = \"init\" ]; then echo 'Initialized!'; exit 0; fi\n" +
		"if [ \"$1\" = \"plan\" ]; then echo 'binary-plan' > tfplan; echo 'Planned!'; exit 0; fi\n" +
		"if [ \"$1\" = \"show\" ] && [ \"$2\" = \"-no-color\" ]; then echo 'Plan: 1 to add, 0 to change, 0 to destroy.'; exit 0; fi\n" +
		"if [ \"$1\" = \"show\" ] && [ \"$2\" = \"-json\" ]; then echo '{\"format_version\":\"1.2\",\"resource_changes\":[{\"type\":\"google_service_account\"}]}'; exit 0; fi\n" +
		"exit 1\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	e := Executor{Binary: staticBinary(fakeBin)}
	res, err := e.Run(context.Background(), Request{
		WorkDir:          dir,
		InternalBaseURL:  "http://127.0.0.1:8080",
		ProjectID:        "p1",
		RunID:            "r1",
		StateToken:       "tok",
		Step:             run.StepPlan,
		TerraformVersion: "1.9.8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Logs)
	}
	if strings.TrimSpace(res.PlanText) != "Plan: 1 to add, 0 to change, 0 to destroy." {
		t.Fatalf("PlanText = %q", res.PlanText)
	}
	if string(res.PlanJSON) != `{"format_version":"1.2","resource_changes":[{"type":"google_service_account"}]}` {
		t.Fatalf("PlanJSON = %q", string(res.PlanJSON))
	}
	if strings.Contains(res.Logs, `"format_version"`) {
		t.Fatalf("raw JSON leaked into human logs: %s", res.Logs)
	}
}
