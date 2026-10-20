package state

import "context"

type Repository interface {
	Load(ctx context.Context, projectID string) (*State, error)
	Save(ctx context.Context, s *State) error
}
