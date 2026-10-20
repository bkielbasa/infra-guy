package run

import (
	"context"
	"errors"
)

var ErrActiveConflict = errors.New("another run for this commit is queued or running")

type Repository interface {
	InsertIfAbsent(ctx context.Context, r *Run) (inserted bool, err error)
	Save(ctx context.Context, r *Run) error
	Get(ctx context.Context, id ID) (*Run, error)
	ListByProject(ctx context.Context, projectID ProjectID) ([]*Run, error)
}
