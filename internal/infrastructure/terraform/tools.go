package terraform

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"infra-guy/internal/domain/project"
)

const maxToolDownloadBytes = 200 << 20

type toolBins struct {
	kubectl, helm, ansiblePlaybook, ansibleGalaxy string
}

type binsCtxKey struct{}

func withBins(ctx context.Context, bins toolBins) context.Context {
	return context.WithValue(ctx, binsCtxKey{}, bins)
}

func binsFrom(ctx context.Context) toolBins {
	bins, _ := ctx.Value(binsCtxKey{}).(toolBins)
	return bins
}

func (b toolBins) resolve(name string) string {
	switch name {
	case "kubectl":
		if b.kubectl != "" {
			return b.kubectl
		}
	case "helm":
		if b.helm != "" {
			return b.helm
		}
	case "ansible-playbook":
		if b.ansiblePlaybook != "" {
			return b.ansiblePlaybook
		}
	case "ansible-galaxy":
		if b.ansibleGalaxy != "" {
			return b.ansibleGalaxy
		}
	}
	return name
}

// ToolInstaller downloads pinned kubectl, Helm, and ansible-core releases into CacheDir.
type ToolInstaller struct {
	CacheDir string
	Client   *http.Client
	mu       sync.Mutex
}

func (t *ToolInstaller) Ensure(ctx context.Context, tool, version string) (string, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		return "", nil
	}
	if t == nil || t.CacheDir == "" {
		return "", fmt.Errorf("%s %s requested but tool cache dir is empty", tool, version)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := os.MkdirAll(t.CacheDir, 0o755); err != nil {
		return "", err
	}
	switch tool {
	case "kubectl":
		return t.ensureKubectl(ctx, version)
	case "helm":
		return t.ensureHelm(ctx, version)
	case "ansible":
		return t.ensureAnsible(ctx, version)
	default:
		return "", fmt.Errorf("unknown tool %q", tool)
	}
}

func (e Executor) resolveToolBins(ctx context.Context, engine, version string, streamer *logStreamer) (toolBins, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" || e.Tools == nil {
		return toolBins{}, nil
	}
	var tool string
	switch engine {
	case project.EngineKubernetes:
		tool = "kubectl"
	case project.EngineHelm:
		tool = "helm"
	case project.EngineAnsible:
		tool = "ansible"
	default:
		return toolBins{}, nil
	}
	streamer.Logf("Resolving %s %s", tool, version)
	path, err := e.Tools.Ensure(WithLogFunc(ctx, streamer.Logf), tool, version)
	if err != nil {
		return toolBins{}, err
	}
	if path == "" {
		return toolBins{}, nil
	}
	bins := toolBins{}
	switch tool {
	case "kubectl":
		bins.kubectl = path
	case "helm":
		bins.helm = path
	case "ansible":
		bins.ansiblePlaybook = path
		bins.ansibleGalaxy = filepath.Join(filepath.Dir(path), "ansible-galaxy")
	}
	streamer.Logf("Using %s %s (%s)", tool, version, path)
	return bins, nil
}

func (t *ToolInstaller) ensureKubectl(ctx context.Context, version string) (string, error) {
	name := "kubectl-" + version
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dest := filepath.Join(t.CacheDir, name)
	if info, err := os.Stat(dest); err == nil && !info.IsDir() {
		return dest, nil
	}
	goos, goarch := runtime.GOOS, runtime.GOARCH
	url := fmt.Sprintf("https://dl.k8s.io/release/v%s/bin/%s/%s/kubectl", version, goos, goarch)
	if goos == "windows" {
		url += ".exe"
	}
	body, err := t.download(ctx, url)
	if err != nil {
		return "", fmt.Errorf("download kubectl %s: %w", version, err)
	}
	if err := os.WriteFile(dest, body, 0o755); err != nil {
		return "", err
	}
	return dest, nil
}

func (t *ToolInstaller) ensureHelm(ctx context.Context, version string) (string, error) {
	dest := filepath.Join(t.CacheDir, "helm-"+version)
	if info, err := os.Stat(dest); err == nil && !info.IsDir() {
		return dest, nil
	}
	url := fmt.Sprintf("https://get.helm.sh/helm-v%s-%s-%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	body, err := t.download(ctx, url)
	if err != nil {
		return "", fmt.Errorf("download helm %s: %w", version, err)
	}
	bin, err := helmBinaryFromTar(body)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, bin, 0o755); err != nil {
		return "", err
	}
	return dest, nil
}

func (t *ToolInstaller) ensureAnsible(ctx context.Context, version string) (string, error) {
	dest := filepath.Join(t.CacheDir, "ansible-core-"+version)
	bin := filepath.Join(dest, "bin", "ansible-playbook")
	if info, err := os.Stat(bin); err == nil && !info.IsDir() {
		return bin, nil
	}
	logProgress(ctx, "Installing ansible-core %s into %s", version, dest)
	cmd := exec.CommandContext(ctx, "python3", "-m", "pip", "install", "--disable-pip-version-check", "--target", dest, "ansible-core=="+version)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("pip install ansible-core==%s: %w\n%s", version, err, out)
	}
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("ansible-playbook missing after installing ansible-core %s", version)
	}
	return bin, nil
}

func (t *ToolInstaller) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxToolDownloadBytes))
}

func helmBinaryFromTar(raw []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "helm" {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, maxToolDownloadBytes))
	}
	return nil, fmt.Errorf("helm binary missing in archive")
}
