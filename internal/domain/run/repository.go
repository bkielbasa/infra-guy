package run

import "context"

type Repository interface {
	InsertIfAbsent(ctx context.Context, r *Run) (inserted bool, err error)
	Save(ctx context.Context, r *Run) error
	Get(ctx context.Context, id ID) (*Run, error)
	ListByProject(ctx context.Context, projectID ProjectID) ([]*Run, error)
}
