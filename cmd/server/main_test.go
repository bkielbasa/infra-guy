package main

import (
	"testing"
)

func TestParseJobID(t *testing.T) {
	emptyEnv := func(string) string { return "" }

	t.Run("default zero when no flag or env", func(t *testing.T) {
		got, err := parseJobID(nil, emptyEnv)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
	})

	t.Run("flag space separated", func(t *testing.T) {
		got, err := parseJobID([]string{"--job-id", "42"}, emptyEnv)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 42 {
			t.Fatalf("got %d, want 42", got)
		}
	})

	t.Run("flag equals separated", func(t *testing.T) {
		got, err := parseJobID([]string{"--job-id=42"}, emptyEnv)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 42 {
			t.Fatalf("got %d, want 42", got)
		}
	})

	t.Run("env var fallback", func(t *testing.T) {
		env := func(k string) string {
			if k == "WORKER_JOB_ID" {
				return "99"
			}
			return ""
		}
		got, err := parseJobID(nil, env)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 99 {
			t.Fatalf("got %d, want 99", got)
		}
	})

	t.Run("flag takes precedence over env var", func(t *testing.T) {
		env := func(k string) string {
			if k == "WORKER_JOB_ID" {
				return "99"
			}
			return ""
		}
		got, err := parseJobID([]string{"--job-id", "42"}, env)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 42 {
			t.Fatalf("got %d, want 42", got)
		}
	})

	t.Run("invalid flag value returns error", func(t *testing.T) {
		if _, err := parseJobID([]string{"--job-id", "not-a-number"}, emptyEnv); err == nil {
			t.Fatal("expected error for non-numeric --job-id")
		}
	})

	t.Run("invalid env value returns error", func(t *testing.T) {
		env := func(k string) string {
			if k == "WORKER_JOB_ID" {
				return "abc"
			}
			return ""
		}
		if _, err := parseJobID(nil, env); err == nil {
			t.Fatal("expected error for non-numeric WORKER_JOB_ID")
		}
	})

	t.Run("negative flag value returns error", func(t *testing.T) {
		if _, err := parseJobID([]string{"--job-id", "-7"}, emptyEnv); err == nil {
			t.Fatal("expected error for negative --job-id")
		}
	})

	t.Run("negative env value returns error", func(t *testing.T) {
		env := func(k string) string {
			if k == "WORKER_JOB_ID" {
				return "-3"
			}
			return ""
		}
		if _, err := parseJobID(nil, env); err == nil {
			t.Fatal("expected error for negative WORKER_JOB_ID")
		}
	})
}

func TestInternalBaseURL(t *testing.T) {
	t.Run("defaults to loopback port from addr", func(t *testing.T) {
		t.Setenv("INTERNAL_BASE_URL", "")
		got := internalBaseURL(":8080")
		if got != "http://127.0.0.1:8080" {
			t.Fatalf("got %q, want %q", got, "http://127.0.0.1:8080")
		}
	})

	t.Run("override via INTERNAL_BASE_URL trims whitespace and trailing slash", func(t *testing.T) {
		t.Setenv("INTERNAL_BASE_URL", "  http://infra-guy:8080/  ")
		got := internalBaseURL(":8080")
		if got != "http://infra-guy:8080" {
			t.Fatalf("got %q, want %q", got, "http://infra-guy:8080")
		}
	})

	t.Run("resolveInternalBaseURL direct calls", func(t *testing.T) {
		if got := resolveInternalBaseURL("0.0.0.0:9090", ""); got != "http://127.0.0.1:9090" {
			t.Fatalf("got %q, want %q", got, "http://127.0.0.1:9090")
		}
		if got := resolveInternalBaseURL(":8080", "http://infra-guy.default.svc:8080/"); got != "http://infra-guy.default.svc:8080" {
			t.Fatalf("got %q, want %q", got, "http://infra-guy.default.svc:8080")
		}
	})
}

func TestBuildWorkerPassThroughEnv(t *testing.T) {
	baseVars := map[string]string{
		"DATABASE_URL":                "postgres://db/infra",
		"PUBLIC_BASE_URL":             "https://infra.example.com",
		"GITHUB_APP_ID":               "12345",
		"GITHUB_APP_PRIVATE_KEY_FILE": "/run/secrets/infra-guy/app.pem",
		"GITHUB_APP_WEBHOOK_SECRET":   "wh-secret",
		"GITHUB_APP_CLIENT_ID":        "cid",
		"GITHUB_APP_CLIENT_SECRET":    "csecret",
		"WORK_DIR":                    "/host/custom/dir",
	}

	t.Run("without CONTEXT_ENCRYPTION_KEY", func(t *testing.T) {
		getenv := func(k string) string { return baseVars[k] }
		got := buildWorkerPassThroughEnv(getenv, "http://infra-guy:8080")

		want := map[string]string{
			"DATABASE_URL":                "postgres://db/infra",
			"PUBLIC_BASE_URL":             "https://infra.example.com",
			"INTERNAL_BASE_URL":           "http://infra-guy:8080",
			"GITHUB_APP_ID":               "12345",
			"GITHUB_APP_PRIVATE_KEY_FILE": "/run/secrets/infra-guy/app.pem",
			"GITHUB_APP_WEBHOOK_SECRET":   "wh-secret",
			"GITHUB_APP_CLIENT_ID":        "cid",
			"GITHUB_APP_CLIENT_SECRET":    "csecret",
			"WORK_DIR":                    "/work",
		}
		if len(got) != len(want) {
			t.Fatalf("len(got) = %d, want %d; got=%v", len(got), len(want), got)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("got[%q] = %q, want %q", k, got[k], v)
			}
		}
		if _, exists := got["CONTEXT_ENCRYPTION_KEY"]; exists {
			t.Errorf("expected CONTEXT_ENCRYPTION_KEY to be omitted when empty")
		}
	})

	t.Run("with CONTEXT_ENCRYPTION_KEY", func(t *testing.T) {
		getenv := func(k string) string {
			if k == "CONTEXT_ENCRYPTION_KEY" {
				return "super-secret-encryption-key"
			}
			return baseVars[k]
		}
		got := buildWorkerPassThroughEnv(getenv, "http://infra-guy:8080")
		if got["CONTEXT_ENCRYPTION_KEY"] != "super-secret-encryption-key" {
			t.Fatalf("got CONTEXT_ENCRYPTION_KEY = %q, want %q", got["CONTEXT_ENCRYPTION_KEY"], "super-secret-encryption-key")
		}
		if got["WORK_DIR"] != "/work" {
			t.Fatalf("got WORK_DIR = %q, want /work", got["WORK_DIR"])
		}
	})

	t.Run("with S3 cache vars", func(t *testing.T) {
		getenv := func(k string) string {
			switch k {
			case "S3_ENDPOINT":
				return "https://s3.internal"
			case "S3_REGION":
				return "eu-central-1"
			case "S3_BUCKET":
				return "infra-guy-cache"
			case "S3_ACCESS_KEY_ID":
				return "ak-123"
			case "S3_SECRET_ACCESS_KEY":
				return "sk-456"
			case "S3_USE_PATH_STYLE":
				return "true"
			case "S3_PREFIX":
				return "workspaces"
			default:
				return baseVars[k]
			}
		}
		got := buildWorkerPassThroughEnv(getenv, "http://infra-guy:8080")
		if got["S3_BUCKET"] != "infra-guy-cache" || got["S3_ENDPOINT"] != "https://s3.internal" || got["S3_ACCESS_KEY_ID"] != "ak-123" || got["S3_SECRET_ACCESS_KEY"] != "sk-456" {
			t.Fatalf("missing S3 pass-through env vars: %v", got)
		}
	})
}
