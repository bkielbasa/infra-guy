package k8s_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra-guy/internal/application/worker"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/infrastructure/k8s"
)

type jobManifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		BackoffLimit            *int32 `json:"backoffLimit"`
		TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished"`
		ActiveDeadlineSeconds   *int64 `json:"activeDeadlineSeconds"`
		Template                struct {
			Metadata struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				RestartPolicy      string `json:"restartPolicy"`
				ServiceAccountName string `json:"serviceAccountName"`
				Containers         []struct {
					Name            string   `json:"name"`
					Image           string   `json:"image"`
					ImagePullPolicy string   `json:"imagePullPolicy"`
					Args            []string `json:"args"`
					EnvFrom         []struct {
						SecretRef struct {
							Name string `json:"name"`
						} `json:"secretRef"`
					} `json:"envFrom"`
					Env []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
					VolumeMounts []struct {
						Name      string `json:"name"`
						MountPath string `json:"mountPath"`
						ReadOnly  bool   `json:"readOnly"`
					} `json:"volumeMounts"`
				} `json:"containers"`
				Volumes []struct {
					Name     string                 `json:"name"`
					EmptyDir map[string]interface{} `json:"emptyDir"`
					Secret   *struct {
						SecretName string `json:"secretName"`
					} `json:"secret"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func TestJobDispatcher_Dispatch_BuildsExpectedJobManifest(t *testing.T) {
	var (
		gotMethod      string
		gotPath        string
		gotAuth        string
		gotContentType string
		gotBody        []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"kind":"Job"}`))
	}))
	defer srv.Close()

	d := &k8s.JobDispatcher{
		APIServerURL:       srv.URL,
		Namespace:          "infra-guy",
		Token:              "test-sa-token",
		Client:             srv.Client(),
		Image:              "ghcr.io/bklimczak/infra-guy:abc1234",
		ImagePullPolicy:    "Always",
		ServiceAccountName: "infra-guy-sa",
		EnvSecretName:      "infra-guy-env",
		PassThroughEnv: map[string]string{
			"WORK_DIR":          "/work",
			"INTERNAL_BASE_URL": "http://infra-guy:8080",
			" PUBLIC_ORDER ":    "ignore", // verify standard keys sort deterministically
		},
	}
	delete(d.PassThroughEnv, " PUBLIC_ORDER ")
	d.PassThroughEnv["A_FIRST"] = "1"
	d.PassThroughEnv["Z_LAST"] = "2"

	job := worker.Job{
		ID:       42,
		RunID:    run.ID("run_123"),
		Step:     run.StepPlan,
		Attempts: 2,
	}

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("Dispatch returned unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/apis/batch/v1/namespaces/infra-guy/jobs" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer test-sa-token" {
		t.Fatalf("unexpected Authorization header: %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("unexpected Content-Type header: %q", gotContentType)
	}

	var manifest jobManifest
	if err := json.Unmarshal(gotBody, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v\nbody: %s", err, string(gotBody))
	}

	if manifest.APIVersion != "batch/v1" || manifest.Kind != "Job" {
		t.Fatalf("unexpected apiVersion/kind: %s / %s", manifest.APIVersion, manifest.Kind)
	}
	if manifest.Metadata.Name != "infra-guy-job-42-2" {
		t.Fatalf("expected job name infra-guy-job-42-2, got %q", manifest.Metadata.Name)
	}
	if manifest.Metadata.Namespace != "infra-guy" {
		t.Fatalf("expected namespace infra-guy, got %q", manifest.Metadata.Namespace)
	}

	expectedLabels := map[string]string{
		"app.kubernetes.io/name": "infra-guy-worker",
		"infra-guy.io/job-id":    "42",
		"infra-guy.io/run-id":    "run_123",
		"infra-guy.io/step":      "plan",
	}
	for k, v := range expectedLabels {
		if manifest.Metadata.Labels[k] != v {
			t.Fatalf("expected metadata label %s=%q, got %q", k, v, manifest.Metadata.Labels[k])
		}
		if manifest.Spec.Template.Metadata.Labels[k] != v {
			t.Fatalf("expected pod template label %s=%q, got %q", k, v, manifest.Spec.Template.Metadata.Labels[k])
		}
	}

	if manifest.Spec.BackoffLimit == nil || *manifest.Spec.BackoffLimit != 0 {
		t.Fatalf("expected backoffLimit=0, got %v", manifest.Spec.BackoffLimit)
	}
	if manifest.Spec.TTLSecondsAfterFinished == nil || *manifest.Spec.TTLSecondsAfterFinished != 300 {
		t.Fatalf("expected ttlSecondsAfterFinished=300, got %v", manifest.Spec.TTLSecondsAfterFinished)
	}
	// StepPlan timeout is 20m = 1200s; + 60s = 1260s
	if manifest.Spec.ActiveDeadlineSeconds == nil || *manifest.Spec.ActiveDeadlineSeconds != 1260 {
		t.Fatalf("expected activeDeadlineSeconds=1260, got %v", manifest.Spec.ActiveDeadlineSeconds)
	}

	if manifest.Spec.Template.Metadata.Annotations["linkerd.io/inject"] != "disabled" {
		t.Fatalf("expected linkerd.io/inject=disabled annotation, got %v", manifest.Spec.Template.Metadata.Annotations)
	}
	if manifest.Spec.Template.Spec.RestartPolicy != "Never" {
		t.Fatalf("expected restartPolicy=Never, got %q", manifest.Spec.Template.Spec.RestartPolicy)
	}
	if manifest.Spec.Template.Spec.ServiceAccountName != "infra-guy-sa" {
		t.Fatalf("expected serviceAccountName=infra-guy-sa, got %q", manifest.Spec.Template.Spec.ServiceAccountName)
	}

	if len(manifest.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(manifest.Spec.Template.Spec.Containers))
	}
	c := manifest.Spec.Template.Spec.Containers[0]
	if c.Name != "worker" {
		t.Fatalf("expected container name worker, got %q", c.Name)
	}
	if c.Image != "ghcr.io/bklimczak/infra-guy:abc1234" {
		t.Fatalf("unexpected container image: %q", c.Image)
	}
	if c.ImagePullPolicy != "Always" {
		t.Fatalf("unexpected imagePullPolicy: %q", c.ImagePullPolicy)
	}
	if len(c.Args) != 2 || c.Args[0] != "--job-id" || c.Args[1] != "42" {
		t.Fatalf("unexpected args: %v", c.Args)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != "infra-guy-env" {
		t.Fatalf("unexpected envFrom: %+v", c.EnvFrom)
	}

	wantEnvKeys := []string{"A_FIRST", "INTERNAL_BASE_URL", "WORK_DIR", "Z_LAST"}
	if len(c.Env) != len(wantEnvKeys) {
		t.Fatalf("expected %d env vars, got %d: %+v", len(wantEnvKeys), len(c.Env), c.Env)
	}
	for i, k := range wantEnvKeys {
		if c.Env[i].Name != k {
			t.Fatalf("env[%d] expected key %q, got %q", i, k, c.Env[i].Name)
		}
	}

	if len(c.VolumeMounts) != 2 {
		t.Fatalf("expected 2 volumeMounts, got %+v", c.VolumeMounts)
	}
	if c.VolumeMounts[0].Name != "work" || c.VolumeMounts[0].MountPath != "/work" || c.VolumeMounts[0].ReadOnly {
		t.Fatalf("unexpected work volumeMount: %+v", c.VolumeMounts[0])
	}
	if c.VolumeMounts[1].Name != "env-secret" || c.VolumeMounts[1].MountPath != "/run/secrets/infra-guy" || !c.VolumeMounts[1].ReadOnly {
		t.Fatalf("unexpected env-secret volumeMount: %+v", c.VolumeMounts[1])
	}

	if len(manifest.Spec.Template.Spec.Volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %+v", manifest.Spec.Template.Spec.Volumes)
	}
	if manifest.Spec.Template.Spec.Volumes[0].Name != "work" || manifest.Spec.Template.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("unexpected work volume: %+v", manifest.Spec.Template.Spec.Volumes[0])
	}
	if manifest.Spec.Template.Spec.Volumes[1].Name != "env-secret" ||
		manifest.Spec.Template.Spec.Volumes[1].Secret == nil ||
		manifest.Spec.Template.Spec.Volumes[1].Secret.SecretName != "infra-guy-env" {
		t.Fatalf("unexpected env-secret volume: %+v", manifest.Spec.Template.Spec.Volumes[1])
	}
}

func TestJobDispatcher_Dispatch_ConflictAndErrorHandling(t *testing.T) {
	t.Run("409 Conflict is treated as idempotent success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"reason":"AlreadyExists"}`))
		}))
		defer srv.Close()

		d := &k8s.JobDispatcher{
			APIServerURL: srv.URL,
			Token:        "tok",
			Image:        "infra-guy:latest",
		}
		err := d.Dispatch(context.Background(), worker.Job{ID: 7, RunID: "run_1", Step: run.StepInit, Attempts: 1})
		if err != nil {
			t.Fatalf("expected nil on 409 Conflict, got %v", err)
		}
	})

	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			}))
			defer srv.Close()

			d := &k8s.JobDispatcher{
				APIServerURL: srv.URL,
				Token:        "tok",
				Image:        "infra-guy:latest",
			}
			err := d.Dispatch(context.Background(), worker.Job{ID: 7, RunID: "run_1", Step: run.StepInit, Attempts: 1})
			if err == nil {
				t.Fatalf("expected error on status %d, got nil", status)
			}
			if !strings.Contains(err.Error(), "boom") {
				t.Fatalf("expected error to include response body 'boom', got %v", err)
			}
		})
	}
}

func TestNewInClusterDispatcher_TLSAndFiles(t *testing.T) {
	var gotAuth string
	var gotPath string

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer tlsSrv.Close()

	tmpDir := t.TempDir()
	tokenFile := filepath.Join(tmpDir, "token")
	if err := os.WriteFile(tokenFile, []byte("rotated-sa-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	nsFile := filepath.Join(tmpDir, "namespace")
	if err := os.WriteFile(nsFile, []byte("custom-ns\n"), 0o600); err != nil {
		t.Fatalf("write namespace file: %v", err)
	}
	caFile := filepath.Join(tmpDir, "ca.crt")
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: tlsSrv.Certificate().Raw,
	})
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatalf("write ca file: %v", err)
	}

	d, err := k8s.NewInClusterDispatcher(k8s.Config{
		Image:         "ghcr.io/bklimczak/infra-guy:latest",
		APIServerURL:  tlsSrv.URL,
		TokenFile:     tokenFile,
		CAFile:        caFile,
		NamespaceFile: nsFile,
	})
	if err != nil {
		t.Fatalf("NewInClusterDispatcher failed: %v", err)
	}

	if d.Namespace != "custom-ns" {
		t.Fatalf("expected namespace 'custom-ns', got %q", d.Namespace)
	}
	if d.ImagePullPolicy != "IfNotPresent" {
		t.Fatalf("expected default ImagePullPolicy 'IfNotPresent', got %q", d.ImagePullPolicy)
	}

	if err := d.Dispatch(context.Background(), worker.Job{ID: 99, RunID: "run_tls", Step: run.StepApply, Attempts: 1}); err != nil {
		t.Fatalf("Dispatch over TLS failed: %v", err)
	}
	if gotAuth != "Bearer rotated-sa-token" {
		t.Fatalf("expected Bearer rotated-sa-token, got %q", gotAuth)
	}
	if gotPath != "/apis/batch/v1/namespaces/custom-ns/jobs" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
}
