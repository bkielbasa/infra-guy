package terraform

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"infra-guy/internal/domain/project"
)

const maxTerraformArchiveBytes = 250 << 20

type BinaryLocator interface {
	Ensure(ctx context.Context, version string) (string, error)
}

type Installer struct {
	CacheDir string
	BaseURL  string
	Client   *http.Client

	mu sync.Mutex
}

func (in *Installer) Ensure(ctx context.Context, version string) (string, error) {
	norm, err := project.ValidateTerraformVersion(version)
	if err != nil {
		return "", err
	}
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.CacheDir != "" {
		cached := filepath.Join(in.CacheDir, "terraform-"+norm)
		if info, err := os.Stat(cached); err == nil && !info.IsDir() {
			return cached, nil
		}
	}

	if in.BaseURL == "" {
		if path, err := exec.LookPath("terraform"); err == nil {
			if got := binaryVersion(ctx, path); got == norm {
				return path, nil
			}
		}
	}

	if in.CacheDir == "" {
		return "", fmt.Errorf("terraform %s not found and cache dir is empty", norm)
	}
	if err := os.MkdirAll(in.CacheDir, 0o755); err != nil {
		return "", err
	}

	base := in.BaseURL
	if base == "" {
		base = "https://releases.hashicorp.com"
	}
	archiveURL := fmt.Sprintf("%s/terraform/%s/terraform_%s_%s_%s.zip", base, norm, norm, runtime.GOOS, runtime.GOARCH)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return "", err
	}
	client := in.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download terraform %s: HTTP %d", norm, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTerraformArchiveBytes))
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", err
	}
	var binEntry *zip.File
	for _, f := range zr.File {
		if filepath.Base(f.Name) == "terraform" {
			binEntry = f
			break
		}
	}
	if binEntry == nil {
		return "", fmt.Errorf("terraform binary missing in archive for %s", norm)
	}
	rc, err := binEntry.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(in.CacheDir, ".tmp-terraform-"+norm+"-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.Copy(tmp, io.LimitReader(rc, maxTerraformArchiveBytes)); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	dest := filepath.Join(in.CacheDir, "terraform-"+norm)
	if err := os.Rename(tmpPath, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func binaryVersion(ctx context.Context, bin string) string {
	out, err := exec.CommandContext(ctx, bin, "version", "-json").CombinedOutput()
	if err != nil {
		return ""
	}
	return parseVersionOutput(out)
}
