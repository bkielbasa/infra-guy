package helmrelease

import "context"

type Revision struct {
	Revision    int
	Updated     string
	Status      string
	Chart       string
	AppVersion  string
	Description string
}

type Repository interface {
	Replace(ctx context.Context, projectID, release, namespace string, revisions []Revision) error
	List(ctx context.Context, projectID string) (release, namespace string, revisions []Revision, err error)
}
