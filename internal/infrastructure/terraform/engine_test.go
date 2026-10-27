package terraform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
)

func TestParseUnifiedDiffAndAnsibleRecap(t *testing.T) {
	diff := "" +
		"diff -u -N /dev/null /tmp/MERGED/apps.v1.Deployment.default.web\n" +
		"--- /dev/null\t2020-01-01\n" +
		"+++ /tmp/MERGED/apps.v1.Deployment.default.web\t2020-01-01\n" +
		"+kind: Deployment\n" +
		"diff -u -N /tmp/LIVE/v1.ConfigMap.default.old /dev/null\n" +
		"--- /tmp/LIVE/v1.ConfigMap.default.old\n" +
		"+++ /dev/null\n"
	got := parseUnifiedDiff(diff, "kubernetes")
	if len(got) != 2 {
		t.Fatalf("resources = %#v", got)
	}
	text, raw := formatEnginePlan(got, diff)
	if !strings.Contains(text, "Plan: 1 to add, 0 to change, 1 to destroy.") {
		t.Fatalf("plan text: %s", text)
	}
	if !strings.Contains(string(raw), `"address":"Deployment.default.web"`) || !strings.Contains(string(raw), `"actions":["delete"]`) {
		t.Fatalf("plan json: %s", raw)
	}

	ansible := "" +
		"TASK [Install nginx]\n" +
		"changed: [web1]\n" +
		"ok: [web2]\n" +
		"PLAY RECAP\n" +
		"web1 : ok=1 changed=1 unreachable=0 failed=0 skipped=0 rescued=0 ignored=0\n" +
		"web2 : ok=1 changed=0 unreachable=1 failed=0 skipped=0 rescued=0 ignored=0\n"
	resources, failed := parseAnsibleChanges(ansible)
	if !failed || len(resources) != 1 || resources[0].Address != "web1: Install nginx" {
		t.Fatalf("ansible = failed:%v %#v", failed, resources)
	}
}

func TestHelmUpgradeArgs(t *testing.T) {
	args := helmUpgradeArgs(project.EngineConfig{
		ReleaseName: "payments", Chart: "./chart", HelmNamespace: "prod",
		ChartVersion: "1.2.3", RepoURL: "https://charts.example.com",
		ValuesFiles: "values.yaml, values/prod.yaml", SetValues: "replicaCount=2",
		Atomic: true, Wait: true, CreateNamespace: true,
	}, false)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"upgrade --install payments ./chart",
		"--namespace prod",
		"--create-namespace",
		"--version 1.2.3",
		"--repo https://charts.example.com",
		"-f values.yaml",
		"-f values/prod.yaml",
		"--set replicaCount=2",
		"--atomic",
		"--wait",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(strings.Join(helmUpgradeArgs(project.EngineConfig{ReleaseName: "payments", Chart: "nginx"}, true), " "), "--wait") {
		t.Fatal("helm diff should not wait")
	}
}

func TestRunEngineKubernetesPlanAndApply(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "diff" ]; then
  echo "diff -u -N /dev/null /tmp/MERGED/apps.v1.Deployment.default.web"
  echo "--- /dev/null"
  echo "+++ /tmp/MERGED/apps.v1.Deployment.default.web"
  exit 1
fi
if [ "$1" = "apply" ] && [ "$2" = "--dry-run=server" ]; then
  printf '%s\n' "apiVersion: apps/v1" "kind: Deployment" "metadata:" "  name: web" "  namespace: default"
  exit 0
fi
if [ "$1" = "apply" ]; then
  echo "deployment.apps/web created"
  exit 0
fi
if [ "$1" = "rollout" ]; then
  echo "deployment \"web\" successfully rolled out"
  exit 0
fi
echo "unexpected: $*" >&2
exit 3
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := t.TempDir()
	ctx := context.Background()
	plan, err := Executor{}.Run(ctx, Request{
		WorkDir: work, Engine: project.EngineKubernetes, Step: run.StepPlan,
		EngineConfig: project.EngineConfig{ManifestPath: "deploy", K8sNamespace: "apps"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ExitCode != 0 || !strings.Contains(plan.PlanText, "Plan: 1 to add, 0 to change, 0 to destroy.") {
		t.Fatalf("plan = %#v", plan)
	}
	if !strings.Contains(string(plan.PlanJSON), "kind: Deployment") {
		t.Fatalf("plan json missing rendered manifest: %s", plan.PlanJSON)
	}
	if !strings.Contains(plan.Logs, "kubectl diff -f deploy -n apps") {
		t.Fatalf("logs: %s", plan.Logs)
	}

	apply, err := Executor{}.Run(ctx, Request{
		WorkDir: work, Engine: project.EngineKubernetes, Step: run.StepApply,
		EngineConfig: project.EngineConfig{ManifestPath: "deploy"},
	})
	if err != nil || apply.ExitCode != 0 || !strings.Contains(apply.Logs, "rollout status deployment/web") {
		t.Fatalf("apply err=%v code=%d logs=%s", err, apply.ExitCode, apply.Logs)
	}

	initRes, err := Executor{}.Run(ctx, Request{
		WorkDir: work, Engine: project.EngineKubernetes, Step: run.StepInit,
	})
	if err != nil || initRes.ExitCode != 0 {
		t.Fatalf("init err=%v %#v", err, initRes)
	}
	if _, err := os.Stat(filepath.Join(work, engineInitMarker)); err != nil {
		t.Fatal(err)
	}
}

func TestRunEngineHelmHistoryAndRollback(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "diff" ]; then
  echo 'Error: unknown command "diff" for "helm"' >&2
  exit 1
fi
if [ "$1" = "template" ]; then
  printf '%s\n' "apiVersion: v1" "kind: ConfigMap" "metadata:" "  name: app" "  namespace: prod"
  exit 0
fi
if [ "$1" = "get" ]; then
  echo "release: not found" >&2
  exit 1
fi
if [ "$1" = "history" ]; then
  echo '[{"revision":1,"updated":"2026-01-01","status":"superseded","chart":"app-1.0.0","app_version":"1.0.0","description":"install"},{"revision":2,"updated":"2026-01-02","status":"deployed","chart":"app-1.1.0","app_version":"1.1.0","description":"upgrade"}]'
  exit 0
fi
if [ "$1" = "rollback" ]; then
  exit 0
fi
echo "unexpected: $*" >&2
exit 3
`
	if err := os.WriteFile(filepath.Join(bin, "helm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := t.TempDir()
	ctx := context.Background()
	cfg := project.EngineConfig{ReleaseName: "payments", HelmNamespace: "prod", Chart: "./chart"}
	plan, err := Executor{}.Run(ctx, Request{WorkDir: work, Engine: project.EngineHelm, Step: run.StepPlan, EngineConfig: cfg})
	if err != nil || plan.ExitCode != 0 || !strings.Contains(plan.PlanText, "Plan: 1 to add, 0 to change, 0 to destroy.") {
		t.Fatalf("plan err=%v %#v", err, plan)
	}
	if len(plan.HelmRevisions) != 2 || plan.HelmRevisions[0].Revision != 2 || plan.HelmRelease != "payments" || plan.HelmNamespace != "prod" {
		t.Fatalf("history = %#v", plan.HelmRevisions)
	}
	rollback, err := Executor{}.Run(ctx, Request{
		WorkDir: work, Engine: project.EngineHelm, Step: run.StepPlan, EngineConfig: cfg, HelmRollback: 1,
	})
	if err != nil || rollback.ExitCode != 0 || !strings.Contains(rollback.PlanText, "Plan: 0 to add, 1 to change, 0 to destroy.") {
		t.Fatalf("rollback plan err=%v text=%s", err, rollback.PlanText)
	}
	apply, err := Executor{}.Run(ctx, Request{
		WorkDir: work, Engine: project.EngineHelm, Step: run.StepApply, EngineConfig: cfg, HelmRollback: 1,
	})
	if err != nil || apply.ExitCode != 0 || !strings.Contains(apply.Logs, "helm rollback payments 1") {
		t.Fatalf("apply err=%v logs=%s", err, apply.Logs)
	}
}

func TestRunEngineAnsibleDestroyRejected(t *testing.T) {
	work := t.TempDir()
	res, err := Executor{}.Run(context.Background(), Request{
		WorkDir: work, Engine: project.EngineAnsible, Step: run.StepApply, Destroy: true,
		EngineConfig: project.EngineConfig{Playbook: "site.yml"},
	})
	if err != nil || res.ExitCode != 1 || !strings.Contains(res.Logs, "does not support destroy") {
		t.Fatalf("err=%v %#v", err, res)
	}
}
