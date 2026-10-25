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

// TenantSummaryRepository is an optional fast-path interface implemented by
// repositories that can fetch lightweight run summaries (without heavy plan
// blobs) for all workspaces in a tenant in a single batch.
type TenantSummaryRepository interface {
	ListSummariesByTenant(ctx context.Context, tenantID string) ([]*Run, error)
}
