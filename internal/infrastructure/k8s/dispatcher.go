package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"infra-guy/internal/application/worker"
)

const (
	defaultTokenFile     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultCAFile        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	defaultNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

var _ worker.Dispatcher = (*JobDispatcher)(nil)

type Config struct {
	Image              string
	ImagePullPolicy    string
	Namespace          string
	ServiceAccountName string
	EnvSecretName      string
	PassThroughEnv     map[string]string
	PodAnnotations     map[string]string

	APIServerURL  string
	TokenFile     string
	CAFile        string
	NamespaceFile string
}

type JobDispatcher struct {
	APIServerURL       string
	Namespace          string
	Token              string
	TokenFile          string
	Client             *http.Client
	Image              string
	ImagePullPolicy    string
	ServiceAccountName string
	EnvSecretName      string
	PassThroughEnv     map[string]string
	PodAnnotations     map[string]string
}

func NewInClusterDispatcher(cfg Config) (*JobDispatcher, error) {
	if strings.TrimSpace(cfg.Image) == "" {
		return nil, errors.New("k8s dispatcher requires a worker container image")
	}

	apiServerURL := strings.TrimSpace(cfg.APIServerURL)
	if apiServerURL == "" {
		host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
		port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
		if host == "" || port == "" {
			return nil, errors.New("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT must be set for in-cluster k8s dispatcher")
		}
		apiServerURL = "https://" + net.JoinHostPort(host, port)
	}

	tokenFile := cfg.TokenFile
	if tokenFile == "" {
		tokenFile = defaultTokenFile
	}
	if _, err := os.ReadFile(tokenFile); err != nil {
		return nil, fmt.Errorf("read serviceaccount token file %s: %w", tokenFile, err)
	}

	caFile := cfg.CAFile
	if caFile == "" {
		caFile = defaultCAFile
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read serviceaccount CA file %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse serviceaccount CA certificates from %s", caFile)
	}

	namespace := strings.TrimSpace(cfg.Namespace)
	if namespace == "" {
		nsFile := cfg.NamespaceFile
		if nsFile == "" {
			nsFile = defaultNamespaceFile
		}
		if data, err := os.ReadFile(nsFile); err == nil {
			namespace = strings.TrimSpace(string(data))
		}
		if namespace == "" {
			namespace = "default"
		}
	}

	pullPolicy := strings.TrimSpace(cfg.ImagePullPolicy)
	if pullPolicy == "" {
		pullPolicy = "IfNotPresent"
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	return &JobDispatcher{
		APIServerURL:       apiServerURL,
		Namespace:          namespace,
		TokenFile:          tokenFile,
		Client:             client,
		Image:              cfg.Image,
		ImagePullPolicy:    pullPolicy,
		ServiceAccountName: cfg.ServiceAccountName,
		EnvSecretName:      cfg.EnvSecretName,
		PassThroughEnv:     cfg.PassThroughEnv,
		PodAnnotations:     cfg.PodAnnotations,
	}, nil
}

func (d *JobDispatcher) Dispatch(ctx context.Context, job worker.Job) error {
	namespace := strings.TrimSpace(d.Namespace)
	if namespace == "" {
		namespace = "default"
	}

	token, err := d.bearerToken()
	if err != nil {
		return err
	}

	pullPolicy := strings.TrimSpace(d.ImagePullPolicy)
	if pullPolicy == "" {
		pullPolicy = "IfNotPresent"
	}

	jobIDStr := strconv.FormatInt(job.ID, 10)
	labels := map[string]string{
		"app.kubernetes.io/name": "infra-guy-worker",
		"infra-guy.io/job-id":    jobIDStr,
		"infra-guy.io/run-id":    string(job.RunID),
		"infra-guy.io/step":      string(job.Step),
	}

	annotations := map[string]string{
		"linkerd.io/inject": "disabled",
	}
	if d.PodAnnotations != nil {
		annotations = make(map[string]string, len(d.PodAnnotations))
		for k, v := range d.PodAnnotations {
			annotations[k] = v
		}
	}

	var envVars []envVar
	if len(d.PassThroughEnv) > 0 {
		keys := make([]string, 0, len(d.PassThroughEnv))
		for k := range d.PassThroughEnv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		envVars = make([]envVar, 0, len(keys))
		for _, k := range keys {
			envVars = append(envVars, envVar{Name: k, Value: d.PassThroughEnv[k]})
		}
	}

	var envFrom []envFromSource
	volumeMounts := []volumeMount{
		{Name: "work", MountPath: "/work"},
	}
	volumes := []volume{
		{Name: "work", EmptyDir: &emptyDirVolumeSource{}},
	}
	if d.EnvSecretName != "" {
		envFrom = append(envFrom, envFromSource{
			SecretRef: &secretEnvSource{Name: d.EnvSecretName},
		})
		volumeMounts = append(volumeMounts, volumeMount{
			Name:      "env-secret",
			MountPath: "/run/secrets/infra-guy",
			ReadOnly:  true,
		})
		volumes = append(volumes, volume{
			Name:   "env-secret",
			Secret: &secretVolumeSource{SecretName: d.EnvSecretName},
		})
	}

	backoffLimit := int32(0)
	ttlSeconds := int32(300)
	activeDeadline := int64(worker.StepTimeout(job.Step).Seconds()) + 60

	payload := batchJob{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: objectMeta{
			Name:      fmt.Sprintf("infra-guy-job-%d-%d", job.ID, job.Attempts),
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: jobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSeconds,
			ActiveDeadlineSeconds:   &activeDeadline,
			Template: podTemplateSpec{
				Metadata: objectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: podSpec{
					RestartPolicy:      "Never",
					ServiceAccountName: d.ServiceAccountName,
					Containers: []container{
						{
							Name:            "worker",
							Image:           d.Image,
							ImagePullPolicy: pullPolicy,
							Args:            []string{"--job-id", jobIDStr},
							EnvFrom:         envFrom,
							Env:             envVars,
							VolumeMounts:    volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal k8s job manifest: %w", err)
	}

	url := fmt.Sprintf("%s/apis/batch/v1/namespaces/%s/jobs", strings.TrimRight(d.APIServerURL, "/"), namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build k8s job request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := d.Client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post k8s job: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusConflict:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	default:
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("k8s job create failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

func (d *JobDispatcher) bearerToken() (string, error) {
	if d.TokenFile != "" {
		b, err := os.ReadFile(d.TokenFile)
		if err != nil {
			return "", fmt.Errorf("read serviceaccount token %s: %w", d.TokenFile, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(d.Token), nil
}

type batchJob struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   objectMeta `json:"metadata"`
	Spec       jobSpec    `json:"spec"`
}

type objectMeta struct {
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type jobSpec struct {
	BackoffLimit            *int32          `json:"backoffLimit"`
	TTLSecondsAfterFinished *int32          `json:"ttlSecondsAfterFinished,omitempty"`
	ActiveDeadlineSeconds   *int64          `json:"activeDeadlineSeconds,omitempty"`
	Template                podTemplateSpec `json:"template"`
}

type podTemplateSpec struct {
	Metadata objectMeta `json:"metadata"`
	Spec     podSpec    `json:"spec"`
}

type podSpec struct {
	RestartPolicy      string      `json:"restartPolicy"`
	ServiceAccountName string      `json:"serviceAccountName,omitempty"`
	Containers         []container `json:"containers"`
	Volumes            []volume    `json:"volumes,omitempty"`
}

type container struct {
	Name            string          `json:"name"`
	Image           string          `json:"image"`
	ImagePullPolicy string          `json:"imagePullPolicy,omitempty"`
	Args            []string        `json:"args,omitempty"`
	EnvFrom         []envFromSource `json:"envFrom,omitempty"`
	Env             []envVar        `json:"env,omitempty"`
	VolumeMounts    []volumeMount   `json:"volumeMounts,omitempty"`
}

type envFromSource struct {
	SecretRef *secretEnvSource `json:"secretRef,omitempty"`
}

type secretEnvSource struct {
	Name string `json:"name"`
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type volumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type volume struct {
	Name     string                `json:"name"`
	EmptyDir *emptyDirVolumeSource `json:"emptyDir,omitempty"`
	Secret   *secretVolumeSource   `json:"secret,omitempty"`
}

type emptyDirVolumeSource struct{}

type secretVolumeSource struct {
	SecretName string `json:"secretName"`
}
