package terraform

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
)

type rewriteTransport struct {
	base string
	next http.RoundTripper
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(r.base)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = u.Scheme
	clone.URL.Host = u.Host
	clone.Host = u.Host
	next := r.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(clone)
}

func TestToolInstallerPinsKubectlAndHelm(t *testing.T) {
	kubectlScript := "#!/bin/sh\nif [ \"$1\" = \"diff\" ]; then\n  echo 'diff -u -N /dev/null /tmp/MERGED/apps.v1.Deployment.default.web'\n  echo '--- /dev/null'\n  echo '+++ /tmp/MERGED/apps.v1.Deployment.default.web'\n  exit 1\nfi\necho pinned-kubectl\nexit 0\n"
	helmBody := []byte("#!/bin/sh\necho pinned-helm\nexit 0\n")
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "helm/helm", Mode: 0o755, Size: int64(len(helmBody))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(helmBody); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	helmArchive := append([]byte(nil), archive.Bytes()...)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "kubectl") {
			_, _ = w.Write([]byte(kubectlScript))
			return
		}
		_, _ = w.Write(helmArchive)
	}))
	t.Cleanup(srv.Close)
	installer := &ToolInstaller{
		CacheDir: t.TempDir(),
		Client:   &http.Client{Transport: rewriteTransport{base: srv.URL}},
	}
	if path, err := installer.Ensure(context.Background(), "kubectl", ""); err != nil || path != "" {
		t.Fatalf("empty version = %q %v", path, err)
	}
	kubectlPath, err := installer.Ensure(context.Background(), "kubectl", "v1.31.2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustRead(t, kubectlPath)), "pinned-kubectl") {
		t.Fatalf("kubectl body missing marker")
	}
	helmPath, err := installer.Ensure(context.Background(), "helm", "3.14.2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustRead(t, helmPath)), "pinned-helm") {
		t.Fatalf("helm archive was not extracted")
	}

	t.Setenv("PATH", t.TempDir())
	plan, err := (Executor{Tools: installer}).Run(context.Background(), Request{
		WorkDir: t.TempDir(), Engine: project.EngineKubernetes, Step: run.StepPlan,
		EngineConfig: project.EngineConfig{ToolVersion: "1.31.2", ManifestPath: "deploy"},
	})
	if err != nil || plan.ExitCode != 0 || !strings.Contains(plan.PlanText, "Plan: 1 to add, 0 to change, 0 to destroy.") {
		t.Fatalf("pinned plan err=%v %#v", err, plan)
	}
	if !strings.Contains(plan.Logs, kubectlPath) {
		t.Fatalf("logs should name the pinned kubectl: %s", plan.Logs)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
