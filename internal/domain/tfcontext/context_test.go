package tfcontext

import (
	"testing"
	"time"
)

func TestValidateEnvKey(t *testing.T) {
	key, err := ValidateEnvKey("  TF_VAR_region ")
	if err != nil || key != "TF_VAR_region" {
		t.Fatalf("got %q, err %v", key, err)
	}
	for _, bad := range []string{
		"", "123_VAR", "FOO-BAR", "FOO=BAR",
		"PATH", "HOME", "USER", "TF_IN_AUTOMATION", "TF_INPUT",
		"DATABASE_URL", "POSTGRES_PASSWORD", "GITHUB_APP_PRIVATE_KEY",
		"CONTEXT_ENCRYPTION_KEY", "TF_LOG", "GIT_TRACE",
	} {
		if _, err := ValidateEnvKey(bad); err == nil {
			t.Fatalf("accepted invalid or reserved env key %q", bad)
		}
	}
}

func TestValidateFilePath(t *testing.T) {
	got, err := ValidateFilePath("~/.kube/config")
	if err != nil || got != ".kube/config" {
		t.Fatalf("got %q, err %v", got, err)
	}
	got2, err := ValidateFilePath("  .aws/credentials ")
	if err != nil || got2 != ".aws/credentials" {
		t.Fatalf("got %q, err %v", got2, err)
	}
	for _, bad := range []string{
		"", "/", "/etc/passwd", "../secret", ".kube/../../etc/passwd",
		"~", "~/", ".", "..", "foo\\bar", "a\x00b",
	} {
		if _, err := ValidateFilePath(bad); err == nil {
			t.Fatalf("accepted unsafe file path %q", bad)
		}
	}
}

func TestResolvePrecedence(t *testing.T) {
	now := time.Unix(10, 0).UTC()
	c1, err := New("c1", "ten-1", "base", now)
	if err != nil {
		t.Fatal(err)
	}
	e1, _ := NewEntry("e1", KindPlainEnv, "TF_VAR_env", "dev", now)
	e2, _ := NewEntry("e2", KindSecretEnv, "AWS_SECRET_ACCESS_KEY", "old-secret", now)
	e3, _ := NewEntry("e3", KindSecretFile, "~/.kube/config", "cluster-a", now)
	c1.SetEntries([]Entry{e1, e2, e3})

	c2, err := New("c2", "ten-1", "prod-override", now)
	if err != nil {
		t.Fatal(err)
	}
	e4, _ := NewEntry("e4", KindPlainEnv, "TF_VAR_env", "prod", now)
	e5, _ := NewEntry("e5", KindSecretFile, ".kube/config", "cluster-b", now)
	c2.SetEntries([]Entry{e4, e5})

	env, files := Resolve([]*Context{c1, c2})
	if env["TF_VAR_env"] != "prod" {
		t.Fatalf("TF_VAR_env = %q, want prod", env["TF_VAR_env"])
	}
	if env["AWS_SECRET_ACCESS_KEY"] != "old-secret" {
		t.Fatalf("AWS_SECRET_ACCESS_KEY = %q, want old-secret", env["AWS_SECRET_ACCESS_KEY"])
	}
	if string(files[".kube/config"]) != "cluster-b" {
		t.Fatalf(".kube/config = %q, want cluster-b", string(files[".kube/config"]))
	}
}
