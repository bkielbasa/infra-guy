package terraform

import (
	"context"

	"infra-guy/internal/application/worker"
)

var _ worker.Executor = WorkerExecutor{}

type WorkerExecutor struct{ Executor }

func (e WorkerExecutor) Run(ctx context.Context, req worker.Request) (worker.Result, error) {
	res, err := e.Executor.Run(ctx, Request{
		WorkDir: req.WorkDir, InternalBaseURL: req.InternalBaseURL, ProjectID: req.ProjectID, RunID: req.RunID,
		StateToken: req.StateToken, WorkingDirectory: req.WorkingDirectory, GitHubToken: req.GitHubToken,
		Engine: req.Engine, EngineConfig: req.EngineConfig,
		TerraformVersion: req.TerraformVersion, HelmRollback: req.HelmRollback,
		Step: req.Step, Destroy: req.Destroy, PlanFile: req.PlanFile,
		Env:           req.Env,
		Files:         req.Files,
		RedactSecrets: req.RedactSecrets,
		OnLog:         req.OnLog,
	})
	return worker.Result{
		ExitCode: res.ExitCode, Logs: res.Logs, PlanText: res.PlanText,
		Version: res.Version, PlanFile: res.PlanFile, PlanJSON: res.PlanJSON,
		HelmRelease: res.HelmRelease, HelmNamespace: res.HelmNamespace, HelmRevisions: res.HelmRevisions,
	}, err
}
