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
		TerraformVersion: req.TerraformVersion,
		Step:             req.Step, PlanFile: req.PlanFile,
		Env:   req.Env,
		Files: req.Files,
	})
	return worker.Result{
		ExitCode: res.ExitCode, Logs: res.Logs, PlanText: res.PlanText,
		Version: res.Version, PlanFile: res.PlanFile, PlanJSON: res.PlanJSON,
	}, err
}
