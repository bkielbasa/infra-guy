package state

import "context"

type Repository interface {
	Load(ctx context.Context, projectID string) (*State, error)
	Save(ctx context.Context, s *State) error
}

// VersionRepository is an optional extension implemented by persistent state
// repositories (such as PostgreSQL) to query and retrieve historical state
// serials without loading every historical state blob on normal Load calls.
type VersionRepository interface {
	ListVersions(ctx context.Context, projectID string) ([]VersionSummary, error)
	GetVersion(ctx context.Context, projectID string, serial int64) (*Version, error)
}
