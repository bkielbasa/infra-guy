package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"infra-guy/internal/domain/helmrelease"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/tfcontext"
)

const WaitDelay = 15 * time.Second
const logFlushInterval = 200 * time.Millisecond

type Executor struct {
	Binary BinaryLocator
	Tools  *ToolInstaller
}

type Request struct {
	WorkDir, InternalBaseURL, ProjectID, RunID, StateToken, WorkingDirectory, GitHubToken string
	Engine                                                                                string
	EngineConfig                                                                          project.EngineConfig
	TerraformVersion                                                                      string
	HelmRollback                                                                          int
	Step                                                                                  run.StepName
	Destroy                                                                               bool
	PlanFile                                                                              []byte
	Env                                                                                   map[string]string
	Files                                                                                 map[string][]byte
	RedactSecrets                                                                         []string
	OnLog                                                                                 func(logs string)
}

type Result struct {
	ExitCode                int
	Logs, PlanText, Version string
	PlanFile                []byte
	PlanJSON                []byte
	HelmRelease             string
	HelmNamespace           string
	HelmRevisions           []helmrelease.Revision
}

var initArgs = []string{"init", "-input=false", "-no-color"}

func commands(step run.StepName, destroy ...bool) [][]string {
	isDestroy := len(destroy) > 0 && destroy[0]
	switch step {
	case run.StepInit:
		return [][]string{initArgs}
	case run.StepPlan:
		planArgs := []string{"plan", "-input=false", "-no-color"}
		if isDestroy {
			planArgs = append(planArgs, "-destroy")
		}
		planArgs = append(planArgs, "-out=tfplan")
		return [][]string{
			planArgs,
			{"show", "-no-color", "tfplan"},
			{"show", "-json", "tfplan"},
		}
	case run.StepApply:
		return [][]string{{"apply", "-input=false", "-no-color", "tfplan"}}
	default:
		return nil
	}
}

func (e Executor) Run(ctx context.Context, req Request) (Result, error) {
	switch strings.ToLower(strings.TrimSpace(req.Engine)) {
	case project.EngineKubernetes, project.EngineAnsible, project.EngineHelm:
		return e.runEngine(ctx, req)
	}
	if req.Step == run.StepApply && len(req.PlanFile) == 0 {
		return Result{ExitCode: 1, Logs: "apply requires a stored plan file"}, nil
	}

	dir := req.WorkDir
	if req.WorkingDirectory != "" {
		dir = filepath.Join(req.WorkDir, req.WorkingDirectory)
	}

	backend := backendFile(req.InternalBaseURL, req.ProjectID, req.RunID, req.StateToken)
	overridePath := filepath.Join(dir, "~infraguy_backend_override.tf.json")
	// A later *_override.tf.json replaces any backend or cloud block in the repository.
	if err := os.WriteFile(overridePath, backend, 0o644); err != nil {
		return Result{}, err
	}
	defer os.Remove(overridePath)

	cmds := commands(req.Step, req.Destroy)
	if cmds == nil {
		return Result{ExitCode: 1, Logs: "unknown terraform step"}, nil
	}
	planPath := filepath.Join(dir, "tfplan")
	defer os.Remove(planPath)
	if req.Step == run.StepApply {
		if err := os.WriteFile(planPath, req.PlanFile, 0o644); err != nil {
			return Result{}, err
		}
	}

	onLog := req.OnLog
	if onLog != nil && len(req.RedactSecrets) > 0 {
		origOnLog := onLog
		onLog = func(logs string) {
			origOnLog(redactText(logs, req.RedactSecrets))
		}
	}
	streamer := newLogStreamer(onLog)
	defer streamer.Close()

	bin := "terraform"
	if e.Binary != nil && req.TerraformVersion != "" {
		var err error
		bin, err = e.Binary.Ensure(WithLogFunc(ctx, streamer.Logf), req.TerraformVersion)
		if err != nil {
			streamer.Close()
			return Result{}, err
		}
	} else {
		streamer.Logf("Using system terraform binary")
	}

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

	var planText string
	var planJSON []byte
	for _, args := range cmds {
		showJSON := len(args) >= 2 && args[0] == "show" && args[1] == "-json"
		showText := !showJSON && len(args) >= 1 && args[0] == "show"

		if !showJSON && !showText {
			streamer.Logf("Running: terraform %s", strings.Join(args, " "))
		}
		cmd := commandBin(ctx, bin, dir, env, args...)
		var cmdOut bytes.Buffer
		if showJSON || showText {
			cmd.Stdout = &cmdOut
			cmd.Stderr = &cmdOut
		} else {
			w := io.MultiWriter(&cmdOut, streamer)
			cmd.Stdout = w
			cmd.Stderr = w
		}

		err := cmd.Run()
		if err != nil {
			if showJSON || showText {
				_, _ = streamer.Write(cmdOut.Bytes())
			}
			streamer.Close()
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return Result{ExitCode: ee.ExitCode(), Logs: redactText(streamer.String(), req.RedactSecrets)}, nil
			}
			return Result{}, err
		}
		if showJSON {
			planJSON = []byte(strings.TrimSpace(cmdOut.String()))
			streamer.Logf("Exported JSON plan (%d bytes)", len(planJSON))
		} else if showText {
			planText = cmdOut.String()
		} else {
			streamer.Flush()
		}
	}
	streamer.Close()

	res := Result{
		ExitCode: 0,
		Logs:     redactText(streamer.String(), req.RedactSecrets),
		PlanText: redactText(planText, req.RedactSecrets),
		PlanJSON: planJSON,
	}
	if req.Step == run.StepPlan {
		plan, err := os.ReadFile(filepath.Join(dir, "tfplan"))
		if err != nil {
			return Result{}, err
		}
		res.PlanFile = plan
	}
	if req.Step == run.StepInit || req.Step == run.StepPlan {
		res.Version = terraformVersionBin(ctx, bin, dir, env)
	}
	return res, nil
}

func redactText(s string, secrets []string) string {
	if s == "" || len(secrets) == 0 {
		return s
	}
	for _, sec := range secrets {
		if len(sec) >= 4 {
			s = strings.ReplaceAll(s, sec, "***REDACTED***")
		}
	}
	return s
}

func materializeFiles(homeDir string, files map[string][]byte) error {
	cleanHome := filepath.Clean(homeDir)
	for relPath, content := range files {
		cleanRel, err := tfcontext.ValidateFilePath(relPath)
		if err != nil {
			return err
		}
		target := filepath.Join(cleanHome, filepath.FromSlash(cleanRel))
		rel, err := filepath.Rel(cleanHome, target)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid file path %q", relPath)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func command(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	return commandBin(ctx, "terraform", dir, env, args...)
}

func commandBin(ctx context.Context, bin, dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = WaitDelay
	return cmd
}

var blockedEnv = map[string]bool{
	"DATABASE_URL":                true,
	"POSTGRES_PASSWORD":           true,
	"BASIC_AUTH_USER":             true,
	"BASIC_AUTH_PASSWORD":         true,
	"GITHUB_APP_ID":               true,
	"GITHUB_APP_PRIVATE_KEY":      true,
	"GITHUB_APP_PRIVATE_KEY_FILE": true,
	"GITHUB_APP_WEBHOOK_SECRET":   true,
	"GITHUB_APP_CLIENT_ID":        true,
	"GITHUB_APP_CLIENT_SECRET":    true,
	"CONTEXT_ENCRYPTION_KEY":      true,
	"ENCRYPTION_KEYRING":          true,
	"ENCRYPTION_ACTIVE_KEY_ID":    true,
	"S3_ENDPOINT":                 true,
	"S3_REGION":                   true,
	"S3_BUCKET":                   true,
	"S3_ACCESS_KEY_ID":            true,
	"S3_SECRET_ACCESS_KEY":        true,
	"S3_USE_PATH_STYLE":           true,
	"S3_PREFIX":                   true,
	"TF_LOG":                      true,
	"GIT_TRACE":                   true,
}

func commandEnv(githubToken, homeDir string, extra map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if blockedEnv[k] || k == "HOME" || (githubToken != "" && v == githubToken) {
			continue
		}
		env = append(env, kv)
	}
	if len(extra) > 0 {
		keys := make([]string, 0, len(extra))
		for k := range extra {
			if blockedEnv[k] || k == "HOME" || k == "PATH" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+extra[k])
		}
	}
	if homeDir != "" {
		env = append(env, "HOME="+homeDir)
		env = expandCredentialPaths(homeDir, env)
	}
	return append(env, "TF_IN_AUTOMATION=1", "TF_INPUT=0")
}

func expandCredentialPaths(home string, env []string) []string {
	keys := map[string]bool{
		"KUBECONFIG":                  true,
		"ANSIBLE_PRIVATE_KEY_FILE":    true,
		"ANSIBLE_VAULT_PASSWORD_FILE": true,
	}
	for i, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !keys[k] || v == "" || filepath.IsAbs(v) || strings.Contains(v, "..") {
			continue
		}
		env[i] = k + "=" + filepath.Join(home, v)
	}
	return env
}

func backendFile(baseURL, projectID, runID, token string) []byte {
	state := strings.TrimRight(baseURL, "/") + "/api/projects/" + projectID + "/state"
	address := state
	if runID != "" {
		address += "?" + url.Values{"run": {runID}}.Encode()
	}
	lock := state + "/lock"
	doc := map[string]any{
		"terraform": []any{
			map[string]any{
				"backend": []any{
					map[string]any{
						"http": map[string]any{
							"address":        address,
							"lock_address":   lock,
							"unlock_address": lock,
							"lock_method":    "LOCK",
							"unlock_method":  "UNLOCK",
							"username":       "terraform",
							"password":       token,
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func terraformVersion(ctx context.Context, dir string, env []string) string {
	return terraformVersionBin(ctx, "terraform", dir, env)
}

func terraformVersionBin(ctx context.Context, bin, dir string, env []string) string {
	out, err := commandBin(ctx, bin, dir, env, "version", "-json").CombinedOutput()
	if err != nil {
		return ""
	}
	return parseVersionOutput(out)
}

func parseVersionOutput(out []byte) string {
	var v struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return ""
	}
	return v.Version
}

type logStreamer struct {
	mu          sync.Mutex
	buf         strings.Builder
	atLineStart bool
	onLog       func(string)
	lastSent    string
	lastAt      time.Time
	notifyCh    chan struct{}
	stopCh      chan struct{}
	doneCh      chan struct{}
	closed      bool
}

func newLogStreamer(onLog func(string)) *logStreamer {
	s := &logStreamer{onLog: onLog, atLineStart: true}
	if onLog == nil {
		return s
	}
	s.notifyCh = make(chan struct{}, 1)
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	go s.loop()
	return s
}

func (s *logStreamer) loop() {
	defer close(s.doneCh)
	ticker := time.NewTicker(logFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-s.notifyCh:
			s.deliver()
		case <-ticker.C:
			s.deliver()
		}
	}
}

func (s *logStreamer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	total := len(p)
	s.mu.Lock()
	ts := time.Now().UTC().Format("15:04:05.000")
	for len(p) > 0 {
		if s.atLineStart {
			_, _ = s.buf.WriteString("[" + ts + "] ")
			s.atLineStart = false
		}
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			_, _ = s.buf.Write(p)
			break
		}
		_, _ = s.buf.Write(p[:idx+1])
		s.atLineStart = true
		p = p[idx+1:]
	}
	shouldNotify := s.onLog != nil && !s.closed && (s.lastAt.IsZero() || time.Since(s.lastAt) >= logFlushInterval)
	s.mu.Unlock()
	if shouldNotify {
		select {
		case s.notifyCh <- struct{}{}:
		default:
		}
	}
	return total, nil
}

func (s *logStreamer) Logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	s.mu.Lock()
	if !s.atLineStart {
		_ = s.buf.WriteByte('\n')
		s.atLineStart = true
	}
	s.mu.Unlock()
	_, _ = s.Write([]byte(msg))
	s.Flush()
}

func (s *logStreamer) deliver() {
	s.mu.Lock()
	if s.onLog == nil {
		s.mu.Unlock()
		return
	}
	cur := s.buf.String()
	if cur == "" || cur == s.lastSent {
		s.mu.Unlock()
		return
	}
	s.lastSent = cur
	s.lastAt = time.Now()
	onLog := s.onLog
	s.mu.Unlock()
	onLog(cur)
}

func (s *logStreamer) Flush() {
	s.mu.Lock()
	if s.onLog == nil || s.closed {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	select {
	case s.notifyCh <- struct{}{}:
	default:
	}
}

func (s *logStreamer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	stopCh := s.stopCh
	doneCh := s.doneCh
	s.mu.Unlock()
	if stopCh != nil {
		close(stopCh)
		<-doneCh
	}
	s.deliver()
}

func (s *logStreamer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
