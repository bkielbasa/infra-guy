package terraform

import (
	"context"

	"infra-guy/internal/application/worker"
)

var _ worker.Executor = WorkerExecutor{}

type WorkerExecutor struct{ Executor }

func (e WorkerExecutor) Run(ctx context.Context, req worker.Request) (worker.Result, error) {
	res, err := e.Executor.Run(ctx, Request{
		WorkDir: req.WorkDir, InternalBaseURL: req.InternalBaseURL, ProjectID: req.ProjectID,
		StateToken: req.StateToken, WorkingDirectory: req.WorkingDirectory,
		Step: req.Step, PlanFile: req.PlanFile,
	})
	return worker.Result{
		ExitCode: res.ExitCode, Logs: res.Logs, PlanText: res.PlanText,
		Version: res.Version, PlanFile: res.PlanFile,
	}, err
}
