package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"infra-guy/internal/domain/helmrelease"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
)

const engineInitMarker = ".infraguy-initialized"

func (e Executor) runEngine(ctx context.Context, req Request) (Result, error) {
	dir := req.WorkDir
	if req.WorkingDirectory != "" {
		dir = filepath.Join(req.WorkDir, req.WorkingDirectory)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}

	onLog := req.OnLog
	if onLog != nil && len(req.RedactSecrets) > 0 {
		orig := onLog
		onLog = func(logs string) { orig(redactText(logs, req.RedactSecrets)) }
	}
	streamer := newLogStreamer(onLog)
	defer streamer.Close()

	homeDir := filepath.Join(req.WorkDir, ".home")
	if err := os.RemoveAll(homeDir); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return Result{}, err
	}
	if err := materializeFiles(homeDir, req.Files); err != nil {
		return Result{}, err
	}
	if len(req.Env) > 0 || len(req.Files) > 0 {
		streamer.Logf("Prepared execution environment (%d env var(s), %d file(s))", len(req.Env), len(req.Files))
	}
	env := commandEnv(req.GitHubToken, homeDir, req.Env)
	eng := strings.ToLower(strings.TrimSpace(req.Engine))
	bins, berr := e.resolveToolBins(ctx, eng, req.EngineConfig.ToolVersion, streamer)
	if berr != nil {
		streamer.Logf("%s", berr.Error())
		streamer.Close()
		return Result{ExitCode: 1, Logs: redactText(streamer.String(), req.RedactSecrets)}, nil
	}
	ctx = withBins(ctx, bins)

	var res Result
	var err error
	switch req.Step {
	case run.StepInit:
		res, err = runEngineInit(ctx, eng, req, dir, env, streamer)
	case run.StepPlan:
		res, err = runEnginePlan(ctx, eng, req, dir, env, streamer)
	case run.StepApply:
		res, err = runEngineApply(ctx, eng, req, dir, env, streamer)
	default:
		streamer.Logf("unknown step %s", req.Step)
		res = Result{ExitCode: 1}
	}
	if err != nil {
		return Result{}, err
	}
	if eng == project.EngineHelm && (req.Step == run.StepPlan || req.Step == run.StepApply) {
		res = attachHelmHistory(ctx, streamer, dir, env, req.EngineConfig, res)
	}
	streamer.Close()
	res.Logs = redactText(streamer.String(), req.RedactSecrets)
	res.PlanText = redactText(res.PlanText, req.RedactSecrets)
	return res, nil
}

func runEngineInit(ctx context.Context, eng string, req Request, dir string, env []string, streamer *logStreamer) (Result, error) {
	streamer.Logf("Initializing %s workspace", project.EngineLabel(eng))
	if eng == project.EngineAnsible {
		if res, err := installGalaxy(ctx, req.EngineConfig, dir, env, streamer); err != nil || res.ExitCode != 0 {
			return res, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, engineInitMarker), []byte(eng+"\n"), 0o644); err != nil {
		return Result{}, err
	}
	streamer.Logf("Workspace initialized")
	return Result{ExitCode: 0}, nil
}

func installGalaxy(ctx context.Context, cfg project.EngineConfig, dir string, env []string, streamer *logStreamer) (Result, error) {
	reqFile := strings.TrimSpace(cfg.RequirementsFile)
	if reqFile == "" {
		reqFile = "requirements.yml"
	}
	if err := workspaceRelative(reqFile); err != nil {
		streamer.Logf("%s", err.Error())
		return Result{ExitCode: 1}, nil
	}
	if _, err := os.Stat(filepath.Join(dir, reqFile)); err != nil {
		streamer.Logf("No requirements file at %s; skipping ansible-galaxy", reqFile)
		return Result{ExitCode: 0}, nil
	}
	collections, err := runCmd(ctx, streamer, dir, env, "ansible-galaxy", "collection", "install", "-r", reqFile)
	if err != nil {
		return Result{}, err
	}
	if collections.ExitCode == 0 {
		return Result{ExitCode: 0}, nil
	}
	streamer.Logf("Collection install failed; trying role requirements")
	roles, err := runCmd(ctx, streamer, dir, env, "ansible-galaxy", "role", "install", "-r", reqFile)
	if err != nil {
		return Result{}, err
	}
	if roles.ExitCode != 0 {
		return Result{ExitCode: roles.ExitCode}, nil
	}
	return Result{ExitCode: 0}, nil
}

func runEnginePlan(ctx context.Context, eng string, req Request, dir string, env []string, streamer *logStreamer) (Result, error) {
	if eng == project.EngineHelm && req.HelmRollback > 0 {
		return planHelmRollback(req, streamer)
	}
	switch eng {
	case project.EngineKubernetes:
		return planKubernetes(ctx, req, dir, env, streamer)
	case project.EngineAnsible:
		return planAnsible(ctx, req, dir, env, streamer)
	case project.EngineHelm:
		return planHelm(ctx, req, dir, env, streamer)
	default:
		streamer.Logf("unsupported engine %q", eng)
		return Result{ExitCode: 1}, nil
	}
}

func runEngineApply(ctx context.Context, eng string, req Request, dir string, env []string, streamer *logStreamer) (Result, error) {
	switch eng {
	case project.EngineKubernetes:
		return applyKubernetes(ctx, req, dir, env, streamer)
	case project.EngineAnsible:
		if req.Destroy {
			streamer.Logf("ansible engine does not support destroy")
			return Result{ExitCode: 1}, nil
		}
		return runPlaybook(ctx, req, dir, env, streamer, false)
	case project.EngineHelm:
		return applyHelm(ctx, req, dir, env, streamer)
	default:
		streamer.Logf("unsupported engine %q", eng)
		return Result{ExitCode: 1}, nil
	}
}

func planKubernetes(ctx context.Context, req Request, dir string, env []string, streamer *logStreamer) (Result, error) {
	cfg := req.EngineConfig
	if err := workspaceRelative(defaultPath(cfg.ManifestPath, ".")); err != nil {
		streamer.Logf("%s", err.Error())
		return Result{ExitCode: 1}, nil
	}
	diffArgs := kubectlArgs("diff", cfg)
	diff, err := runCmd(ctx, streamer, dir, env, "kubectl", diffArgs...)
	if err != nil {
		return Result{}, err
	}
	if diff.ExitCode != 0 && (diff.ExitCode != 1 || !looksLikeDiff(diff.Output)) {
		return Result{ExitCode: diff.ExitCode}, nil
	}
	rendered := ""
	dryArgs := kubectlArgs("apply", cfg)
	dryArgs = append([]string{"apply", "--dry-run=server", "-o", "yaml"}, dryArgs[1:]...)
	dry, err := runCmd(ctx, streamer, dir, env, "kubectl", dryArgs...)
	if err != nil {
		return Result{}, err
	}
	if dry.ExitCode == 0 {
		rendered = dry.Output
	} else {
		streamer.Logf("Server dry-run did not produce manifests (exit %d); using kubectl diff only", dry.ExitCode)
	}
	resources := mergeManifests(parseUnifiedDiff(diff.Output, "kubernetes"), splitYAMLDocs(rendered))
	text, raw := formatEnginePlan(resources, diff.Output)
	return Result{ExitCode: 0, PlanText: text, PlanJSON: raw, PlanFile: raw}, nil
}

func applyKubernetes(ctx context.Context, req Request, dir string, env []string, streamer *logStreamer) (Result, error) {
	cfg := req.EngineConfig
	if err := workspaceRelative(defaultPath(cfg.ManifestPath, ".")); err != nil {
		streamer.Logf("%s", err.Error())
		return Result{ExitCode: 1}, nil
	}
	binArgs := kubectlArgs("apply", cfg)
	if req.Destroy {
		binArgs[0] = "delete"
		binArgs = append(binArgs, "--ignore-not-found=true")
	} else if cfg.K8sPrune {
		binArgs = append(binArgs, "--prune", "-l", "app.kubernetes.io/managed-by=infra-guy")
	}
	applied, err := runCmd(ctx, streamer, dir, env, "kubectl", binArgs...)
	if err != nil {
		return Result{}, err
	}
	if applied.ExitCode != 0 {
		return Result{ExitCode: applied.ExitCode}, nil
	}
	if req.Destroy {
		return Result{ExitCode: 0}, nil
	}
	for _, target := range rolloutTargets(applied.Output) {
		args := []string{"rollout", "status", target, "--timeout=5m"}
		if ns := strings.TrimSpace(cfg.K8sNamespace); ns != "" {
			args = append(args, "-n", ns)
		}
		st, err := runCmd(ctx, streamer, dir, env, "kubectl", args...)
		if err != nil {
			return Result{}, err
		}
		if st.ExitCode != 0 {
			return Result{ExitCode: st.ExitCode}, nil
		}
	}
	return Result{ExitCode: 0}, nil
}

func planAnsible(ctx context.Context, req Request, dir string, env []string, streamer *logStreamer) (Result, error) {
	res, err := runPlaybook(ctx, req, dir, env, streamer, true)
	if err != nil || res.ExitCode != 0 {
		return res, err
	}
	resources, failed := parseAnsibleChanges(res.PlanText)
	if failed {
		streamer.Logf("Ansible check reported failed or unreachable hosts")
		return Result{ExitCode: 2, PlanText: res.PlanText}, nil
	}
	text, raw := formatEnginePlan(resources, res.PlanText)
	return Result{ExitCode: 0, PlanText: text, PlanJSON: raw, PlanFile: raw}, nil
}

func runPlaybook(ctx context.Context, req Request, dir string, env []string, streamer *logStreamer, check bool) (Result, error) {
	cfg := req.EngineConfig
	playbook := strings.TrimSpace(cfg.Playbook)
	if playbook == "" {
		playbook = "site.yml"
	}
	if err := workspaceRelative(playbook); err != nil {
		streamer.Logf("%s", err.Error())
		return Result{ExitCode: 1}, nil
	}
	if err := workspaceRelative(cfg.Inventory); err != nil {
		streamer.Logf("%s", err.Error())
		return Result{ExitCode: 1}, nil
	}
	args := []string{}
	if check {
		args = append(args, "--check", "--diff")
	}
	if inv := strings.TrimSpace(cfg.Inventory); inv != "" {
		args = append(args, "-i", inv)
	}
	if cfg.Become {
		args = append(args, "--become")
	}
	for _, extra := range splitList(cfg.ExtraVars) {
		args = append(args, "-e", extra)
	}
	args = append(args, playbook)
	ran, err := runCmd(ctx, streamer, dir, env, "ansible-playbook", args...)
	if err != nil {
		return Result{}, err
	}
	if ran.ExitCode != 0 {
		return Result{ExitCode: ran.ExitCode, PlanText: ran.Output}, nil
	}
	return Result{ExitCode: 0, PlanText: ran.Output}, nil
}

func planHelm(ctx context.Context, req Request, dir string, env []string, streamer *logStreamer) (Result, error) {
	cfg := req.EngineConfig
	if err := validateHelmRuntime(cfg); err != nil {
		streamer.Logf("%s", err.Error())
		return Result{ExitCode: 1}, nil
	}
	diffArgs := helmUpgradeArgs(cfg, true)
	diff, err := runCmd(ctx, streamer, dir, env, "helm", diffArgs...)
	if err != nil {
		return Result{}, err
	}
	if diff.ExitCode == 0 || (diff.ExitCode == 1 && looksLikeDiff(diff.Output)) {
		resources := parseUnifiedDiff(diff.Output, "helm")
		text, raw := formatEnginePlan(resources, diff.Output)
		return Result{ExitCode: 0, PlanText: text, PlanJSON: raw, PlanFile: raw}, nil
	}
	if !missingHelmDiff(diff.Output) && diff.ExitCode != 127 {
		return Result{ExitCode: diff.ExitCode}, nil
	}
	streamer.Logf("helm diff plugin is not installed; comparing helm template with the current release")
	return planHelmTemplate(ctx, cfg, dir, env, streamer)
}

func planHelmTemplate(ctx context.Context, cfg project.EngineConfig, dir string, env []string, streamer *logStreamer) (Result, error) {
	desired, err := runCmd(ctx, streamer, dir, env, "helm", helmTemplateArgs(cfg)...)
	if err != nil {
		return Result{}, err
	}
	if desired.ExitCode != 0 {
		return Result{ExitCode: desired.ExitCode}, nil
	}
	current := ""
	got, err := runCmd(ctx, streamer, dir, env, "helm", helmGetManifestArgs(cfg)...)
	if err != nil {
		return Result{}, err
	}
	if got.ExitCode == 0 {
		current = got.Output
	} else {
		streamer.Logf("No current Helm release; treating the plan as an install")
	}
	resources := diffManifestDocs(current, desired.Output, "helm")
	text, raw := formatEnginePlan(resources, desired.Output)
	return Result{ExitCode: 0, PlanText: text, PlanJSON: raw, PlanFile: raw}, nil
}

func applyHelm(ctx context.Context, req Request, dir string, env []string, streamer *logStreamer) (Result, error) {
	cfg := req.EngineConfig
	if req.HelmRollback > 0 {
		if strings.TrimSpace(cfg.ReleaseName) == "" {
			streamer.Logf("release name is required")
			return Result{ExitCode: 1}, nil
		}
		args := []string{"rollback", cfg.ReleaseName, strconv.Itoa(req.HelmRollback)}
		if ns := strings.TrimSpace(cfg.HelmNamespace); ns != "" {
			args = append(args, "--namespace", ns)
		}
		if cfg.Wait || cfg.Atomic {
			args = append(args, "--wait")
		}
		args = append(args, "--timeout", "10m0s")
		ran, err := runCmd(ctx, streamer, dir, env, "helm", args...)
		if err != nil {
			return Result{}, err
		}
		return Result{ExitCode: ran.ExitCode}, nil
	}
	if strings.TrimSpace(cfg.ReleaseName) == "" {
		streamer.Logf("release name is required")
		return Result{ExitCode: 1}, nil
	}
	var args []string
	if req.Destroy {
		args = []string{"uninstall", cfg.ReleaseName}
		if ns := strings.TrimSpace(cfg.HelmNamespace); ns != "" {
			args = append(args, "--namespace", ns)
		}
	} else {
		if err := validateHelmRuntime(cfg); err != nil {
			streamer.Logf("%s", err.Error())
			return Result{ExitCode: 1}, nil
		}
		args = helmUpgradeArgs(cfg, false)
	}
	ran, err := runCmd(ctx, streamer, dir, env, "helm", args...)
	if err != nil {
		return Result{}, err
	}
	return Result{ExitCode: ran.ExitCode}, nil
}

func planHelmRollback(req Request, streamer *logStreamer) (Result, error) {
	release := strings.TrimSpace(req.EngineConfig.ReleaseName)
	if release == "" {
		streamer.Logf("release name is required")
		return Result{ExitCode: 1}, nil
	}
	detail := fmt.Sprintf("Helm rollback %s to revision %d", release, req.HelmRollback)
	streamer.Logf("%s", detail)
	resources := []engineResource{{
		Address: "helm." + release, Kind: "HelmRelease", Name: release, Provider: "helm",
		Action: "update", Manifest: detail,
	}}
	text, raw := formatEnginePlan(resources, detail)
	return Result{ExitCode: 0, PlanText: text, PlanJSON: raw, PlanFile: raw}, nil
}

func attachHelmHistory(ctx context.Context, streamer *logStreamer, dir string, env []string, cfg project.EngineConfig, res Result) Result {
	release := strings.TrimSpace(cfg.ReleaseName)
	if release == "" {
		return res
	}
	ns := strings.TrimSpace(cfg.HelmNamespace)
	args := []string{"history", release, "-o", "json"}
	if ns != "" {
		args = append(args, "--namespace", ns)
	}
	ran, err := runCmd(ctx, streamer, dir, env, "helm", args...)
	if err != nil || ran.ExitCode != 0 {
		streamer.Logf("helm history is not available yet")
		return res
	}
	res.HelmRelease = release
	res.HelmNamespace = ns
	res.HelmRevisions = parseHelmHistory(ran.Output)
	return res
}

func parseHelmHistory(output string) []helmrelease.Revision {
	start := strings.Index(output, "[")
	end := strings.LastIndex(output, "]")
	if start < 0 || end < start {
		return nil
	}
	var raw []struct {
		Revision    int    `json:"revision"`
		Updated     string `json:"updated"`
		Status      string `json:"status"`
		Chart       string `json:"chart"`
		AppVersion  string `json:"app_version"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(output[start:end+1]), &raw); err != nil {
		return nil
	}
	out := make([]helmrelease.Revision, 0, len(raw))
	for _, item := range raw {
		if item.Revision <= 0 {
			continue
		}
		out = append(out, helmrelease.Revision{
			Revision: item.Revision, Updated: item.Updated, Status: item.Status,
			Chart: item.Chart, AppVersion: item.AppVersion, Description: item.Description,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Revision > out[j].Revision })
	return out
}

func kubectlArgs(verb string, cfg project.EngineConfig) []string {
	path := defaultPath(cfg.ManifestPath, ".")
	args := []string{verb}
	if cfg.Kustomize {
		args = append(args, "-k", path)
	} else {
		args = append(args, "-f", path)
	}
	if ns := strings.TrimSpace(cfg.K8sNamespace); ns != "" {
		args = append(args, "-n", ns)
	}
	return args
}

func helmUpgradeArgs(cfg project.EngineConfig, diff bool) []string {
	args := []string{}
	if diff {
		args = append(args, "diff")
	}
	args = append(args, "upgrade", "--install", cfg.ReleaseName, cfg.Chart)
	args = append(args, helmCommonArgs(cfg)...)
	if !diff {
		if cfg.Atomic {
			args = append(args, "--atomic")
		}
		if cfg.Wait || cfg.Atomic {
			args = append(args, "--wait")
		}
		args = append(args, "--timeout", "10m0s")
	}
	return args
}

func helmTemplateArgs(cfg project.EngineConfig) []string {
	args := []string{"template", cfg.ReleaseName, cfg.Chart}
	return append(args, helmCommonArgs(cfg)...)
}

func helmGetManifestArgs(cfg project.EngineConfig) []string {
	args := []string{"get", "manifest", cfg.ReleaseName}
	if ns := strings.TrimSpace(cfg.HelmNamespace); ns != "" {
		args = append(args, "--namespace", ns)
	}
	return args
}

func helmCommonArgs(cfg project.EngineConfig) []string {
	var args []string
	if ns := strings.TrimSpace(cfg.HelmNamespace); ns != "" {
		args = append(args, "--namespace", ns)
	}
	if cfg.CreateNamespace {
		args = append(args, "--create-namespace")
	}
	if v := strings.TrimSpace(cfg.ChartVersion); v != "" {
		args = append(args, "--version", v)
	}
	if repo := strings.TrimSpace(cfg.RepoURL); repo != "" {
		args = append(args, "--repo", repo)
	}
	for _, f := range splitList(cfg.ValuesFiles) {
		args = append(args, "-f", f)
	}
	for _, s := range splitList(cfg.SetValues) {
		args = append(args, "--set", s)
	}
	return args
}

func validateHelmRuntime(cfg project.EngineConfig) error {
	if strings.TrimSpace(cfg.ReleaseName) == "" {
		return errors.New("release name is required")
	}
	if strings.TrimSpace(cfg.Chart) == "" {
		return errors.New("chart is required")
	}
	for _, f := range splitList(cfg.ValuesFiles) {
		if err := workspaceRelative(f); err != nil {
			return err
		}
	}
	chart := strings.TrimSpace(cfg.Chart)
	if strings.HasPrefix(chart, "./") || strings.HasPrefix(chart, "../") {
		return workspaceRelative(chart)
	}
	return nil
}

func defaultPath(raw, fallback string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return fallback
	}
	return v
}

func splitList(raw string) []string {
	raw = strings.ReplaceAll(raw, "\n", ",")
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func workspaceRelative(raw string) error {
	v := strings.TrimSpace(raw)
	if v == "" || v == "." {
		return nil
	}
	if strings.HasPrefix(v, "-") || filepath.IsAbs(v) {
		return fmt.Errorf("path %q must be relative to the working directory", raw)
	}
	clean := filepath.Clean(v)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes the workspace", raw)
	}
	return nil
}

func missingHelmDiff(output string) bool {
	s := strings.ToLower(output)
	return strings.Contains(s, "unknown command") && strings.Contains(s, "diff")
}

func looksLikeDiff(output string) bool {
	trim := strings.TrimSpace(output)
	return strings.Contains(output, "diff -") || strings.Contains(output, "\n--- ") || strings.HasPrefix(trim, "--- ")
}

type cmdResult struct {
	ExitCode int
	Output   string
}

func runCmd(ctx context.Context, streamer *logStreamer, dir string, env []string, name string, args ...string) (cmdResult, error) {
	bin := binsFrom(ctx).resolve(name)
	if filepath.IsAbs(bin) {
		if _, err := os.Stat(bin); err != nil {
			streamer.Logf("%s not found at %s", name, bin)
			return cmdResult{ExitCode: 127, Output: name + " not found"}, nil
		}
	} else if _, err := exec.LookPath(bin); err != nil {
		streamer.Logf("%s not found on PATH", name)
		return cmdResult{ExitCode: 127, Output: name + " not found on PATH"}, nil
	}
	streamer.Logf("Running: %s %s", name, strings.Join(args, " "))
	cmd := commandBin(ctx, bin, dir, env, args...)
	var buf bytes.Buffer
	w := io.MultiWriter(&buf, streamer)
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	streamer.Flush()
	if err == nil {
		return cmdResult{ExitCode: 0, Output: buf.String()}, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return cmdResult{ExitCode: ee.ExitCode(), Output: buf.String()}, nil
	}
	return cmdResult{}, err
}

var rolloutLine = regexp.MustCompile(`(?m)^(deployment|statefulset|daemonset)\.apps/(\S+)`)

func rolloutTargets(output string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range rolloutLine.FindAllStringSubmatch(output, -1) {
		target := m[1] + "/" + m[2]
		if seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
}

type engineResource struct {
	Address  string
	Kind     string
	Name     string
	Provider string
	Action   string
	Manifest string
}

func formatEnginePlan(resources []engineResource, detail string) (string, []byte) {
	sort.Slice(resources, func(i, j int) bool {
		if resources[i].Address == resources[j].Address {
			return resources[i].Action < resources[j].Action
		}
		return resources[i].Address < resources[j].Address
	})
	add, change, destroy := 0, 0, 0
	for _, r := range resources {
		switch r.Action {
		case "delete":
			destroy++
		case "update":
			change++
		default:
			add++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plan: %d to add, %d to change, %d to destroy.\n", add, change, destroy)
	if strings.TrimSpace(detail) != "" {
		b.WriteString(strings.TrimSpace(detail))
		b.WriteByte('\n')
	}
	raw, _ := json.Marshal(map[string]any{
		"format_version":   "1.2",
		"resource_changes": engineResourceChanges(resources),
	})
	return b.String(), raw
}

func engineResourceChanges(resources []engineResource) []map[string]any {
	out := make([]map[string]any, 0, len(resources))
	for _, r := range resources {
		action := r.Action
		if action == "" {
			action = "create"
		}
		var before, after any
		manifest := map[string]any{"manifest": r.Manifest}
		switch action {
		case "delete":
			before = manifest
		case "update":
			before = map[string]any{}
			after = manifest
		default:
			action = "create"
			after = manifest
		}
		name := r.Name
		if name == "" {
			name = r.Address
		}
		kind := r.Kind
		if kind == "" {
			kind = "resource"
		}
		out = append(out, map[string]any{
			"address":       r.Address,
			"mode":          "managed",
			"type":          kind,
			"name":          name,
			"provider_name": r.Provider,
			"change": map[string]any{
				"actions": []string{action},
				"before":  before,
				"after":   after,
			},
		})
	}
	return out
}

func parseUnifiedDiff(text, provider string) []engineResource {
	lines := strings.Split(text, "\n")
	var resources []engineResource
	var from, to string
	var hunk strings.Builder
	flush := func() {
		if from == "" && to == "" {
			hunk.Reset()
			return
		}
		action := "update"
		path := to
		switch {
		case isDevNull(from) && !isDevNull(to):
			action = "create"
			path = to
		case !isDevNull(from) && isDevNull(to):
			action = "delete"
			path = from
		}
		kind, ns, name := resourceIdentity(path)
		addr := kind
		if ns != "" {
			addr += "." + ns
		}
		if name != "" {
			if addr != "" {
				addr += "."
			}
			addr += name
		}
		if addr == "" {
			addr = filepath.Base(path)
			kind = addr
			name = addr
		}
		resources = append(resources, engineResource{
			Address: addr, Kind: kind, Name: name, Provider: provider,
			Action: action, Manifest: strings.TrimSpace(hunk.String()),
		})
		from, to = "", ""
		hunk.Reset()
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "diff ") {
			flush()
			continue
		}
		if strings.HasPrefix(line, "--- ") {
			from = firstToken(line[4:])
			hunk.WriteString(line)
			hunk.WriteByte('\n')
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			to = firstToken(line[4:])
		}
		if from != "" || to != "" {
			hunk.WriteString(line)
			hunk.WriteByte('\n')
		}
	}
	flush()
	return resources
}

func isDevNull(path string) bool {
	return path == "" || path == "/dev/null" || strings.HasSuffix(path, "/dev/null")
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

func resourceIdentity(path string) (kind, namespace, name string) {
	base := filepath.Base(strings.TrimSpace(path))
	if base == "" || base == "." || base == "/dev/null" || base == "null" {
		return "", "", ""
	}
	parts := strings.Split(base, ".")
	if len(parts) == 1 {
		return parts[0], "", parts[0]
	}
	name = parts[len(parts)-1]
	if len(parts) >= 4 {
		namespace = parts[len(parts)-2]
		kind = parts[len(parts)-3]
		return kind, namespace, name
	}
	if len(parts) == 3 {
		return parts[1], "", name
	}
	return parts[0], "", name
}

func mergeManifests(resources []engineResource, docs map[string]string) []engineResource {
	for i := range resources {
		if manifest, ok := docs[resources[i].Address]; ok && resources[i].Action != "delete" {
			resources[i].Manifest = manifest
		}
	}
	return resources
}

func splitYAMLDocs(raw string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	for _, doc := range strings.Split(raw, "\n---") {
		doc = strings.TrimSpace(doc)
		if doc == "" || doc == "---" {
			continue
		}
		kind, ns, name := scanYAMLMeta(doc)
		if kind == "" || name == "" {
			continue
		}
		addr := kind
		if ns != "" {
			addr += "." + ns
		}
		addr += "." + name
		out[addr] = doc
	}
	return out
}

func scanYAMLMeta(doc string) (kind, namespace, name string) {
	inMeta := false
	for _, line := range strings.Split(doc, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inMeta = strings.HasPrefix(trim, "metadata:")
			if strings.HasPrefix(trim, "kind:") {
				kind = strings.TrimSpace(strings.TrimPrefix(trim, "kind:"))
				kind = strings.Trim(kind, `"'`)
			}
			continue
		}
		if !inMeta {
			continue
		}
		if strings.HasPrefix(trim, "name:") && name == "" {
			name = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trim, "name:")), `"'`)
		}
		if strings.HasPrefix(trim, "namespace:") && namespace == "" {
			namespace = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trim, "namespace:")), `"'`)
		}
	}
	return kind, namespace, name
}

func diffManifestDocs(current, desired, provider string) []engineResource {
	from := splitYAMLDocs(current)
	to := splitYAMLDocs(desired)
	var resources []engineResource
	seen := map[string]bool{}
	for addr, doc := range to {
		seen[addr] = true
		kind, _, name := scanYAMLMeta(doc)
		action := "create"
		if prev, ok := from[addr]; ok {
			if strings.TrimSpace(prev) == strings.TrimSpace(doc) {
				continue
			}
			action = "update"
		}
		resources = append(resources, engineResource{
			Address: addr, Kind: kind, Name: name, Provider: provider, Action: action, Manifest: doc,
		})
	}
	for addr, doc := range from {
		if seen[addr] {
			continue
		}
		kind, _, name := scanYAMLMeta(doc)
		resources = append(resources, engineResource{
			Address: addr, Kind: kind, Name: name, Provider: provider, Action: "delete", Manifest: doc,
		})
	}
	return resources
}

var (
	ansibleTask    = regexp.MustCompile(`^TASK \[(.+)]`)
	ansibleChanged = regexp.MustCompile(`^changed: \[([^]]+)]`)
	ansibleRecap   = regexp.MustCompile(`^(\S+)\s+:\s+ok=\d+\s+changed=(\d+)\s+unreachable=(\d+)\s+failed=(\d+)`)
)

func parseAnsibleChanges(output string) ([]engineResource, bool) {
	var resources []engineResource
	task := ""
	failed := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if m := ansibleTask.FindStringSubmatch(line); m != nil {
			task = m[1]
			continue
		}
		if m := ansibleChanged.FindStringSubmatch(line); m != nil {
			name := task
			if name == "" {
				name = "task"
			}
			host := m[1]
			resources = append(resources, engineResource{
				Address: host + ": " + name, Kind: "task", Name: name, Provider: "ansible",
				Action: "update", Manifest: line,
			})
			continue
		}
		if m := ansibleRecap.FindStringSubmatch(line); m != nil {
			if m[3] != "0" || m[4] != "0" {
				failed = true
			}
		}
	}
	if len(resources) == 0 {
		for _, line := range strings.Split(output, "\n") {
			m := ansibleRecap.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil || m[2] == "0" {
				continue
			}
			resources = append(resources, engineResource{
				Address: m[1], Kind: "host", Name: m[1], Provider: "ansible",
				Action: "update", Manifest: line,
			})
		}
	}
	return resources, failed
}
