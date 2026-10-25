package cloudintegration

import (
	"context"

	"infra-guy/internal/domain/project"
)

type Repository interface {
	Save(ctx context.Context, c *Integration) error
	Get(ctx context.Context, id ID) (*Integration, error)
	ListByTenant(ctx context.Context, tenantID string) ([]*Integration, error)
	Delete(ctx context.Context, id ID) error
	SetProjectIntegrations(ctx context.Context, projectID project.ID, ids []ID) error
	ListByProject(ctx context.Context, projectID project.ID) ([]*Integration, error)
}
