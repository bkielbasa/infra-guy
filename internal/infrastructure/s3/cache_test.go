package s3

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCacheSaveRestoreAndDeleteRoundTrip(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	var authHeaders []string
	var contentHashes []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		contentHashes = append(contentHashes, r.Header.Get("X-Amz-Content-Sha256"))

		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			objects[r.URL.Path] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			data, ok := objects[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/zip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		case http.MethodDelete:
			delete(objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)

	cache, err := New(Config{
		Endpoint:        srv.URL,
		Region:          "eu-central-1",
		Bucket:          "infra-guy-cache",
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		UsePathStyle:    true,
		Client:          srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	srcDir := t.TempDir()
	// Create a fake repo + .terraform provider binary + lockfile + ephemeral files that must be skipped.
	providerDir := filepath.Join(srcDir, ".terraform", "providers", "registry.terraform.io", "hashicorp", "null", "3.2.2", "linux_amd64")
	if err := os.MkdirAll(providerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	providerBin := filepath.Join(providerDir, "terraform-provider-null_v3.2.2_x5")
	if err := os.WriteFile(providerBin, []byte("#!/bin/sh\necho provider\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "main.tf"), []byte("resource \"null_resource\" \"x\" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".terraform.lock.hcl"), []byte("# lockfile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ephemeral secrets/files that must NEVER be archived into S3:
	if err := os.MkdirAll(filepath.Join(srcDir, ".home", ".kube"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".home", ".kube", "config"), []byte("secret-kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "~infraguy_backend_override.tf.json"), []byte("secret-state-token"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "tfplan"), []byte("secret-plan-binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	key := "ten-1/proj-1/run-1"

	if err := cache.Save(ctx, key, srcDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	mu.Lock()
	rawZip, exists := objects["/infra-guy-cache/workspaces/ten-1/proj-1/run-1.zip"]
	mu.Unlock()
	if !exists || len(rawZip) == 0 {
		t.Fatalf("expected zip object in S3, got objects=%v", objects)
	}
	if bytes.Contains(rawZip, []byte("secret-kubeconfig")) || bytes.Contains(rawZip, []byte("secret-state-token")) || bytes.Contains(rawZip, []byte("secret-plan-binary")) {
		t.Fatal("zip archive leaked ephemeral secrets")
	}
	for _, h := range authHeaders {
		if !strings.HasPrefix(h, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/") {
			t.Fatalf("unexpected Authorization header: %q", h)
		}
	}
	for _, ch := range contentHashes {
		if len(ch) != 64 {
			t.Fatalf("expected 64-char sha256 hex digest, got %q", ch)
		}
	}

	destDir := t.TempDir()
	ok, err := cache.Restore(ctx, key, destDir)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !ok {
		t.Fatal("Restore returned ok=false, want true")
	}

	restoredMain, err := os.ReadFile(filepath.Join(destDir, "main.tf"))
	if err != nil || string(restoredMain) != "resource \"null_resource\" \"x\" {}\n" {
		t.Fatalf("restored main.tf = %q, err = %v", restoredMain, err)
	}
	restoredBin := filepath.Join(destDir, ".terraform", "providers", "registry.terraform.io", "hashicorp", "null", "3.2.2", "linux_amd64", "terraform-provider-null_v3.2.2_x5")
	info, err := os.Stat(restoredBin)
	if err != nil {
		t.Fatalf("stat restored provider binary: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("restored provider binary lost executable bit: mode=%v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(destDir, ".home")); !os.IsNotExist(err) {
		t.Fatalf(".home should not be in restored workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "~infraguy_backend_override.tf.json")); !os.IsNotExist(err) {
		t.Fatalf("backend override should not be in restored workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "tfplan")); !os.IsNotExist(err) {
		t.Fatalf("tfplan should not be in restored workspace: %v", err)
	}

	// Delete removes the object from S3
	if err := cache.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ok, err = cache.Restore(ctx, key, t.TempDir())
	if err != nil {
		t.Fatalf("Restore after Delete: %v", err)
	}
	if ok {
		t.Fatal("expected Restore to return ok=false after Delete")
	}
}

func TestNewFromEnvDisabledWhenBucketEmpty(t *testing.T) {
	c, err := NewFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Fatalf("expected nil cache when S3_BUCKET is unset, got %#v", c)
	}
}

func TestUnzipRejectsZipSlip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, err := zw.Create("../escape.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("pwn"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	raw := buf.Bytes()
	err = unzipToDir(bytes.NewReader(raw), int64(len(raw)), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsafe zip entry path") {
		t.Fatalf("expected unsafe zip entry path error, got %v", err)
	}
}

