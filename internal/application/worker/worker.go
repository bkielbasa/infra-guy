package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cloudintegrationapp "infra-guy/internal/application/cloudintegration"
	runapp "infra-guy/internal/application/run"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/infrastructure/telemetry"
)

const (
	HeartbeatEvery  = 20 * time.Second
	StaleAfter      = 2 * time.Minute
	ClaimRetryDelay = 5 * time.Second
	MaxStepAttempts = 2
)

func StepTimeout(step run.StepName) time.Duration {
	switch step {
	case run.StepPlan:
		return 20 * time.Minute
	case run.StepApply:
		return 60 * time.Minute
	default:
		return 10 * time.Minute
	}
}

type Job = run.Job

func ReapPolicy(job run.Job) run.ReapDecision {
	if job.Step == run.StepApply {
		return run.ReapDecision{
			Fail:   true,
			Reason: "worker lost the apply; check state before starting another run",
		}
	}
	if job.Attempts >= MaxStepAttempts {
		return run.ReapDecision{
			Fail:   true,
			Reason: "worker lost the step",
		}
	}
	return run.ReapDecision{Fail: false}
}

type Request struct {
	WorkDir, InternalBaseURL, ProjectID, RunID, StateToken, WorkingDirectory, GitHubToken string
	TerraformVersion                                                                      string
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
}

type Queue interface {
	Claim(ctx context.Context, workerID string, retryDelay time.Duration) (*run.Job, error)
	Get(ctx context.Context, jobID int64) (*run.Job, error)
	Heartbeat(ctx context.Context, jobID int64) error
	Complete(ctx context.Context, jobID int64) error
	Reap(ctx context.Context, staleAfter time.Duration, decide func(run.Job) run.ReapDecision) (requeue []run.Job, failed []run.Job, err error)
}

type Dispatcher interface {
	Dispatch(ctx context.Context, job Job) error
}

type Checkout interface {
	Clone(ctx context.Context, dest, cloneURL, token, sha string) error
}

type Executor interface {
	Run(ctx context.Context, req Request) (Result, error)
}

type TokenSource interface {
	InstallationToken(ctx context.Context, tenantID string) (string, error)
}

type ContextResolver interface {
	ResolveForProject(ctx context.Context, projectID project.ID) (map[string]string, map[string][]byte, error)
}

type DependencyResolver interface {
	ResolveForProject(ctx context.Context, projectID project.ID) (map[string]string, []string, error)
}

type CloudCredentialResolver interface {
	ResolveCredentialsForRun(ctx context.Context, p project.Snapshot, runID, branch, sha, trigger, homeDir string) (cloudintegrationapp.RunCredentials, error)
}

type WorkspaceCache interface {
	Save(ctx context.Context, key, srcDir string) error
	Restore(ctx context.Context, key, destDir string) (bool, error)
	Delete(ctx context.Context, key string) error
}

type ProjectGetter interface {
	Get(ctx context.Context, id project.ID) (*project.Project, error)
}

type ProjectLister interface {
	List(ctx context.Context, tenantID string) ([]*project.Project, error)
}

type BranchTipper interface {
	BranchTip(ctx context.Context, cloneURL, token, branch string) (string, error)
}

func WorkspaceCacheKey(tenantID, projectID, runID string) string {
	return path.Join(tenantID, projectID, runID)
}

type Worker struct {
	WorkerID          string
	Queue             Queue
	Projects          ProjectGetter
	Service           *runapp.Service
	Checkout          Checkout
	Executor          Executor
	Dispatcher        Dispatcher
	Tokens            TokenSource
	Contexts          ContextResolver
	Dependencies      DependencyResolver
	CloudIntegrations CloudCredentialResolver
	Cache             WorkspaceCache
	WorkDir           string
	InternalBaseURL   string
	Now               func() time.Time
	Timeout           func(run.StepName) time.Duration

	lastLifecycleCheck time.Time
}

func (w *Worker) Loop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := w.Tick(ctx); err != nil {
			log.Printf("worker tick: %v", err)
		}
		now := time.Now().UTC()
		if w.Now != nil {
			now = w.Now().UTC()
		}
		if w.lastLifecycleCheck.IsZero() || now.Sub(w.lastLifecycleCheck) >= 30*time.Second {
			w.lastLifecycleCheck = now
			if err := w.TickLifecycle(ctx); err != nil {
				log.Printf("worker lifecycle tick: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) Tick(ctx context.Context) error {
	_, failed, err := w.Queue.Reap(ctx, StaleAfter, ReapPolicy)
	if err != nil {
		return err
	}
	for _, job := range failed {
		if err := w.Service.Fail(ctx, job.RunID, job.Step, ReapPolicy(job).Reason); err != nil {
			return err
		}
		if err := w.removeIfTerminal(ctx, job.RunID); err != nil {
			return err
		}
	}

	job, err := w.Queue.Claim(ctx, w.WorkerID, ClaimRetryDelay)
	if err != nil || job == nil {
		return err
	}

	if w.Dispatcher != nil {
		if err := w.Dispatcher.Dispatch(ctx, *job); err != nil {
			if ferr := w.Service.Fail(ctx, job.RunID, job.Step, "dispatch worker job: "+err.Error()); ferr != nil {
				return ferr
			}
			return w.completeAndCleanup(ctx, *job)
		}
		return nil
	}

	hbCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.heartbeat(hbCtx, job.ID)
	return w.runJob(ctx, *job)
}

func (w *Worker) ExecuteClaimed(ctx context.Context, jobID int64) error {
	job, err := w.Queue.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %d not found or not running", jobID)
	}
	_ = w.Queue.Heartbeat(ctx, job.ID)
	hbCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.heartbeat(hbCtx, job.ID)
	return w.runJob(ctx, *job)
}

func (w *Worker) heartbeat(ctx context.Context, jobID int64) {
	ticker := time.NewTicker(HeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.Queue.Heartbeat(ctx, jobID)
		}
	}
}

func (w *Worker) runJob(ctx context.Context, job Job) (retErr error) {
	ctx, jobSpan := telemetry.Default().StartSpan(ctx, "worker.run_job",
		telemetry.Attr("run.id", string(job.RunID)),
		telemetry.Attr("run.step", string(job.Step)),
		telemetry.Attr("worker.id", w.WorkerID),
	)
	defer func() {
		if retErr != nil {
			jobSpan.RecordError(retErr)
		}
		jobSpan.End()
	}()

	r, err := w.Service.BeginStep(ctx, job.RunID, job.Step)
	if err != nil {
		return err
	}

	p, err := w.Projects.Get(ctx, project.ID(r.Snapshot().ProjectID))
	if err != nil || p == nil {
		return fmt.Errorf("project %s not found", r.Snapshot().ProjectID)
	}
	ps := p.Snapshot()
	rs := r.Snapshot()
	op := "apply"
	if rs.IsDestroy {
		op = "destroy"
	} else if rs.PlanOnly {
		op = "plan"
	}
	jobSpan.SetAttribute("tenant.id", ps.TenantID)
	jobSpan.SetAttribute("project.id", string(ps.ID))
	jobSpan.SetAttribute("project.name", ps.Name)
	jobSpan.SetAttribute("run.operation", op)
	jobSpan.SetAttribute("run.trigger", string(rs.Trigger))

	now := time.Now().UTC()
	if w.Now != nil {
		now = w.Now().UTC()
	}
	if job.Step == run.StepInit && !rs.CreatedAt.IsZero() && !now.Before(rs.CreatedAt) {
		telemetry.Default().RecordRunQueueWait(ps.TenantID, string(ps.ID), ps.Name, op, now.Sub(rs.CreatedAt))
	}

	if w.Tokens == nil {
		if ferr := w.Service.Fail(ctx, job.RunID, job.Step, "missing token source"); ferr != nil {
			return ferr
		}
		return w.completeAndCleanup(ctx, job)
	}
	token, err := w.Tokens.InstallationToken(ctx, ps.TenantID)
	if err != nil {
		if ferr := w.Service.Fail(ctx, job.RunID, job.Step, err.Error()); ferr != nil {
			return ferr
		}
		return w.completeAndCleanup(ctx, job)
	}

	if job.Step == run.StepApply && len(rs.PlanFile) == 0 {
		if err := w.Service.Fail(ctx, job.RunID, job.Step, "apply requires a stored plan file"); err != nil {
			return err
		}
		return w.completeAndCleanup(ctx, job)
	}

	dest := filepath.Join(w.WorkDir, string(job.RunID))
	cacheKey := WorkspaceCacheKey(ps.TenantID, string(ps.ID), string(job.RunID))
	if w.Cache != nil {
		defer func() { _ = os.RemoveAll(dest) }()
	}
	logger := &stepLogger{
		save: func(logs string) {
			_ = w.Service.UpdateStepLogs(ctx, job.RunID, job.Step, logs)
		},
	}
	prepCtx, prepSpan := telemetry.Default().StartSpan(ctx, "worker.prepare_workspace",
		telemetry.Attr("run.id", string(job.RunID)),
		telemetry.Attr("run.step", string(job.Step)),
	)
	prepErr := w.prepareWorkspace(prepCtx, job, ps, rs, token, dest, cacheKey, logger)
	if prepErr != nil {
		prepSpan.RecordError(prepErr)
	}
	prepSpan.End()
	if prepErr != nil {
		if ferr := w.Service.Fail(ctx, job.RunID, job.Step, prepErr.Error()); ferr != nil {
			return ferr
		}
		return w.completeAndCleanup(ctx, job)
	}

	var ctxEnv map[string]string
	var ctxFiles map[string][]byte
	if w.Contexts != nil {
		logger.LogBeforef("Resolving project contexts...")
		var cerr error
		ctxEnv, ctxFiles, cerr = w.Contexts.ResolveForProject(ctx, ps.ID)
		if cerr != nil {
			if ferr := w.Service.Fail(ctx, job.RunID, job.Step, cerr.Error()); ferr != nil {
				return ferr
			}
			return w.completeAndCleanup(ctx, job)
		}
		logger.LogBeforef("Resolved %d environment variable(s) and %d file(s)", len(ctxEnv), len(ctxFiles))
	}
	if w.Dependencies != nil {
		depEnv, depMsgs, derr := w.Dependencies.ResolveForProject(ctx, ps.ID)
		if derr != nil {
			if ferr := w.Service.Fail(ctx, job.RunID, job.Step, derr.Error()); ferr != nil {
				return ferr
			}
			return w.completeAndCleanup(ctx, job)
		}
		for _, msg := range depMsgs {
			logger.LogBeforef("%s", msg)
		}
		if len(depEnv) > 0 {
			if ctxEnv == nil {
				ctxEnv = make(map[string]string, len(depEnv))
			}
			for k, v := range depEnv {
				ctxEnv[k] = v
			}
		}
	}

	var redactSecrets []string
	if w.CloudIntegrations != nil {
		credCtx, credSpan := telemetry.Default().StartSpan(ctx, "worker.cloud_credentials",
			telemetry.Attr("run.id", string(job.RunID)),
			telemetry.Attr("project.id", string(ps.ID)),
		)
		homeDir := filepath.Join(dest, ".home")
		creds, cerr := w.CloudIntegrations.ResolveCredentialsForRun(credCtx, ps, string(job.RunID), rs.Branch, rs.SHA, string(rs.Trigger), homeDir)
		if cerr != nil {
			credSpan.RecordError(cerr)
		}
		credSpan.End()
		if cerr != nil {
			if ferr := w.Service.Fail(ctx, job.RunID, job.Step, cerr.Error()); ferr != nil {
				return ferr
			}
			return w.completeAndCleanup(ctx, job)
		}
		for _, msg := range creds.Summary {
			logger.LogBeforef("%s", msg)
		}
		if len(creds.Env) > 0 {
			if ctxEnv == nil {
				ctxEnv = make(map[string]string, len(creds.Env))
			}
			for k, v := range creds.Env {
				ctxEnv[k] = v
			}
		}
		if len(creds.Files) > 0 {
			if ctxFiles == nil {
				ctxFiles = make(map[string][]byte, len(creds.Files))
			}
			for k, v := range creds.Files {
				ctxFiles[k] = v
			}
		}
		redactSecrets = append(redactSecrets, creds.RedactSecrets...)
	}

	timeout := StepTimeout(job.Step)
	if w.Timeout != nil {
		timeout = w.Timeout(job.Step)
	}
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tfCtx, tfSpan := telemetry.Default().StartSpan(stepCtx, "terraform."+string(job.Step),
		telemetry.Attr("run.id", string(job.RunID)),
		telemetry.Attr("run.step", string(job.Step)),
		telemetry.Attr("project.id", string(ps.ID)),
		telemetry.Attr("project.name", ps.Name),
		telemetry.Attr("terraform.version", ps.TerraformVersion),
	)
	execStart := time.Now()
	res, err := w.Executor.Run(tfCtx, Request{
		WorkDir: dest, InternalBaseURL: w.InternalBaseURL, ProjectID: string(ps.ID), RunID: string(job.RunID),
		StateToken: ps.StateToken, WorkingDirectory: ps.WorkingDirectory, GitHubToken: token,
		TerraformVersion: ps.TerraformVersion, Step: job.Step, Destroy: rs.IsDestroy, PlanFile: rs.PlanFile,
		Env: ctxEnv, Files: ctxFiles, RedactSecrets: redactSecrets,
		OnLog: func(logs string) {
			logger.SetExec(logs)
		},
	})
	execDur := time.Since(execStart)
	if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
		tfSpan.RecordError(stepCtx.Err())
		tfSpan.End()
		telemetry.Default().RecordRunDuration(ps.TenantID, string(ps.ID), ps.Name, op, "failed", string(job.Step), execDur)
		if ferr := w.Service.Fail(ctx, job.RunID, job.Step, "step timed out"); ferr != nil {
			return ferr
		}
		return w.completeAndCleanup(ctx, job)
	}
	if err != nil {
		tfSpan.RecordError(err)
		tfSpan.End()
		telemetry.Default().RecordRunDuration(ps.TenantID, string(ps.ID), ps.Name, op, "failed", string(job.Step), execDur)
		if ferr := w.Service.Fail(ctx, job.RunID, job.Step, err.Error()); ferr != nil {
			return ferr
		}
		return w.completeAndCleanup(ctx, job)
	}
	stepStatus := "succeeded"
	if res.ExitCode != 0 && !(job.Step == run.StepPlan && res.ExitCode == 2) {
		stepStatus = "failed"
	}
	tfSpan.SetAttribute("terraform.exit_code", fmt.Sprintf("%d", res.ExitCode))
	tfSpan.End()
	telemetry.Default().RecordRunDuration(ps.TenantID, string(ps.ID), ps.Name, op, stepStatus, string(job.Step), execDur)
	logger.SetExec(res.Logs)
	if job.Step == run.StepInit && res.ExitCode == 0 && w.Cache != nil {
		logger.LogAfterf("Uploading initialized workspace cache to S3 (%s)...", cacheKey)
		if err := w.Cache.Save(ctx, cacheKey, dest); err != nil {
			if ferr := w.Service.Fail(ctx, job.RunID, job.Step, "save workspace cache: "+err.Error()); ferr != nil {
				return ferr
			}
			return w.completeAndCleanup(ctx, job)
		}
		logger.LogAfterf("Workspace cache uploaded")
	}
	if res.Version != "" {
		if err := w.Service.SetTerraformVersion(ctx, job.RunID, res.Version); err != nil {
			return err
		}
	}
	if err := w.Service.Advance(ctx, job.RunID, job.Step, run.StepResult{
		ExitCode: res.ExitCode, Logs: logger.String(), PlanText: res.PlanText, PlanFile: res.PlanFile, PlanJSON: res.PlanJSON,
	}); err != nil {
		return err
	}
	return w.completeAndCleanup(ctx, job)
}

func (w *Worker) prepareWorkspace(ctx context.Context, job Job, ps project.Snapshot, rs run.Snapshot, token, dest, cacheKey string, logger *stepLogger) error {
	sha := rs.SHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	if w.Cache != nil {
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
		if job.Step == run.StepInit {
			logger.LogBeforef("Cloning repository %s@%s...", ps.Repo.String(), sha)
			if err := w.Checkout.Clone(ctx, dest, ps.Repo.CloneURL(), token, rs.SHA); err != nil {
				return err
			}
			logger.LogBeforef("Repository cloned")
			return nil
		}
		logger.LogBeforef("Restoring workspace cache from S3 (%s)...", cacheKey)
		ok, err := w.Cache.Restore(ctx, cacheKey, dest)
		if err != nil {
			return fmt.Errorf("restore workspace cache: %w", err)
		}
		if !ok {
			return errors.New("workspace cache not found for run")
		}
		logger.LogBeforef("Workspace cache restored")
		return nil
	}

	if job.Step != run.StepInit && hasInitializedTerraform(dest, ps.WorkingDirectory) {
		logger.LogBeforef("Reusing initialized local workspace")
		return nil
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	logger.LogBeforef("Cloning repository %s@%s...", ps.Repo.String(), sha)
	if err := w.Checkout.Clone(ctx, dest, ps.Repo.CloneURL(), token, rs.SHA); err != nil {
		return err
	}
	logger.LogBeforef("Repository cloned")
	return nil
}

type stepLogger struct {
	mu     sync.Mutex
	before strings.Builder
	exec   string
	after  strings.Builder
	save   func(string)
}

func (l *stepLogger) LogBeforef(format string, args ...any) {
	l.mu.Lock()
	writeTimestampedLines(&l.before, fmt.Sprintf(format, args...))
	combined := l.lockedString()
	l.mu.Unlock()
	if l.save != nil {
		l.save(combined)
	}
}

func (l *stepLogger) SetExec(logs string) {
	l.mu.Lock()
	l.exec = logs
	combined := l.lockedString()
	l.mu.Unlock()
	if l.save != nil {
		l.save(combined)
	}
}

func (l *stepLogger) LogAfterf(format string, args ...any) {
	l.mu.Lock()
	writeTimestampedLines(&l.after, fmt.Sprintf(format, args...))
	combined := l.lockedString()
	l.mu.Unlock()
	if l.save != nil {
		l.save(combined)
	}
}

func (l *stepLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lockedString()
}

func (l *stepLogger) lockedString() string {
	b := l.before.String()
	e := l.exec
	if e != "" && !strings.HasSuffix(e, "\n") && l.after.Len() > 0 {
		e += "\n"
	}
	return b + e + l.after.String()
}

func writeTimestampedLines(buf *strings.Builder, msg string) {
	ts := time.Now().UTC().Format("15:04:05.000")
	for _, line := range strings.Split(strings.TrimSuffix(msg, "\n"), "\n") {
		buf.WriteString("[" + ts + "] " + line + "\n")
	}
}

func hasInitializedTerraform(dest, workingDir string) bool {
	dir := dest
	if workingDir != "" {
		dir = filepath.Join(dest, workingDir)
	}
	info, err := os.Stat(filepath.Join(dir, ".terraform"))
	return err == nil && info.IsDir()
}

func (w *Worker) completeAndCleanup(ctx context.Context, job Job) error {
	if err := w.Queue.Complete(ctx, job.ID); err != nil {
		return err
	}
	return w.removeIfTerminal(ctx, job.RunID)
}

func (w *Worker) removeIfTerminal(ctx context.Context, id run.ID) error {
	r, err := w.Service.Get(ctx, id)
	if err != nil || r == nil {
		return err
	}
	rs := r.Snapshot()
	if rs.Status.Terminal() {
		if w.Cache != nil && w.Projects != nil {
			if p, perr := w.Projects.Get(ctx, project.ID(rs.ProjectID)); perr == nil && p != nil {
				ps := p.Snapshot()
				_ = w.Cache.Delete(ctx, WorkspaceCacheKey(ps.TenantID, string(ps.ID), string(id)))
			}
		}
		return os.RemoveAll(filepath.Join(w.WorkDir, string(id)))
	}
	return nil
}

// TickLifecycle evaluates all workspaces for scheduled drift checks and TTL expiration auto-destroy runs.
func (w *Worker) TickLifecycle(ctx context.Context) error {
	if w == nil || w.Projects == nil || w.Service == nil {
		return nil
	}
	lister, ok := w.Projects.(ProjectLister)
	if !ok {
		return nil
	}
	projects, err := lister.List(ctx, "")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if w.Now != nil {
		now = w.Now().UTC()
	}

	for _, p := range projects {
		if p == nil {
			continue
		}
		ps := p.Snapshot()
		if ps.Locked {
			continue
		}
		if !p.IsExpired(now) && !p.DueForDriftCheck(now) {
			continue
		}

		runs, err := w.Service.ListByProject(ctx, ps.ID)
		if err != nil {
			continue
		}
		hasActiveRun := false
		for _, r := range runs {
			if !r.Snapshot().Status.Terminal() {
				hasActiveRun = true
				break
			}
		}
		if hasActiveRun {
			continue
		}

		if p.IsExpired(now) {
			alreadyTriggeredTTL := false
			for _, r := range runs {
				rs := r.Snapshot()
				if rs.Trigger == run.TriggerTTL && ps.ExpiresAt != nil && !rs.CreatedAt.Before(*ps.ExpiresAt) {
					alreadyTriggeredTTL = true
					break
				}
			}
			if !alreadyTriggeredTTL {
				sha := w.resolveProjectCommit(ctx, ps, runs)
				if sha != "" {
					autoApprove := true
					_, _ = w.Service.Open(ctx, runapp.OpenRequest{
						ProjectID:   ps.ID,
						Trigger:     run.TriggerTTL,
						SHA:         sha,
						Branch:      ps.DefaultBranch,
						Message:     "TTL expired: automatic ephemeral workspace teardown",
						Author:      "infra-guy",
						TriggeredBy: "ttl_scheduler",
						IsDestroy:   true,
						AutoApprove: &autoApprove,
					})
				}
			}
			continue
		}

		if p.DueForDriftCheck(now) {
			interval := time.Duration(ps.DriftIntervalMinutes) * time.Minute
			if interval <= 0 {
				interval = 24 * time.Hour
			}
			recentDrift := false
			for _, r := range runs {
				rs := r.Snapshot()
				if rs.Trigger == run.TriggerDrift && now.Sub(rs.CreatedAt) < interval {
					recentDrift = true
					break
				}
			}
			if !recentDrift {
				sha := w.resolveProjectCommit(ctx, ps, runs)
				if sha != "" {
					_, _ = w.Service.Open(ctx, runapp.OpenRequest{
						ProjectID:   ps.ID,
						Trigger:     run.TriggerDrift,
						SHA:         sha,
						Branch:      ps.DefaultBranch,
						Message:     "Scheduled drift detection check",
						Author:      "infra-guy",
						TriggeredBy: "drift_scheduler",
						PlanOnly:    true,
					})
				}
			}
		}
	}
	return nil
}

func (w *Worker) resolveProjectCommit(ctx context.Context, ps project.Snapshot, runs []*run.Run) string {
	if tipper, ok := w.Checkout.(BranchTipper); ok && w.Tokens != nil {
		if token, err := w.Tokens.InstallationToken(ctx, ps.TenantID); err == nil && token != "" {
			if sha, err := tipper.BranchTip(ctx, ps.Repo.CloneURL(), token, ps.DefaultBranch); err == nil && sha != "" {
				return sha
			}
		}
	}
	for _, r := range runs {
		rs := r.Snapshot()
		if rs.SHA != "" && (rs.Branch == ps.DefaultBranch || rs.Branch == "") {
			return rs.SHA
		}
	}
	for _, r := range runs {
		if rs := r.Snapshot(); rs.SHA != "" {
			return rs.SHA
		}
	}
	return ""
}
