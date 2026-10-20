package project

import "context"

type Repository interface {
	Save(ctx context.Context, p *Project) error
	SetLocked(ctx context.Context, id ID, locked bool) error
	ReleaseActiveRun(ctx context.Context, id ID, runID string) error
	Get(ctx context.Context, id ID) (*Project, error)
	List(ctx context.Context) ([]*Project, error)
	Delete(ctx context.Context, id ID) error
}
