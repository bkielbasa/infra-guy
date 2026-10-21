package installation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"infra-guy/internal/domain/tenant"
)

var (
	ErrNotInstalled = errors.New("github app not installed")
	errNotInstalled = ErrNotInstalled
)

// Repo is a GitHub repository accessible to an installation.
type Repo struct {
	ID    int64
	Owner string
	Name  string
}

// InstallationEvent is the parsed GitHub App installation webhook event.
type InstallationEvent struct {
	Action         string
	InstallationID int64
	AccountID      int64
	AccountLogin   string
	AccountType    string
	Suspended      bool
}

// Store matches the identity persistence layer used by the installation service.
type Store interface {
	UpsertTenant(ctx context.Context, t tenant.Tenant, now time.Time) (tenant.Tenant, error)
	UpsertInstallation(ctx context.Context, in tenant.Installation, now time.Time) error
	ActiveInstallation(ctx context.Context, tenantID string) (*tenant.Installation, error)
	InstallationByGitHubID(ctx context.Context, githubInstallationID int64) (*tenant.Installation, error)
	DeactivateInstallation(ctx context.Context, githubInstallationID int64, now time.Time) error
}

// Client is the GitHub App client used to fetch repos.
type Client interface {
	ListRepos(ctx context.Context, installationID int64) ([]Repo, error)
}

type Service struct {
	Store  Store
	GitHub Client
	Now    func() time.Time
	NewID  func() string
}

func (s *Service) Apply(ctx context.Context, event string, payload InstallationEvent) error {
	now := s.now()
	switch payload.Action {
	case "created", "unsuspend":
		accountType, err := tenant.ParseAccountType(payload.AccountType)
		if err != nil {
			return err
		}
		if s.NewID == nil {
			return fmt.Errorf("missing id generator")
		}
		ten, err := s.Store.UpsertTenant(ctx, tenant.Tenant{
			ID:              s.NewID(),
			Name:            payload.AccountLogin,
			AccountType:     accountType,
			GitHubAccountID: payload.AccountID,
		}, now)
		if err != nil {
			return err
		}
		return s.Store.UpsertInstallation(ctx, tenant.Installation{
			ID:                   s.NewID(),
			TenantID:             ten.ID,
			AccountLogin:         payload.AccountLogin,
			AccountType:          accountType,
			GitHubInstallationID: payload.InstallationID,
			Active:               true,
		}, now)
	case "deleted", "suspend":
		if err := s.Store.DeactivateInstallation(ctx, payload.InstallationID, now); err != nil {
			return err
		}
		if payload.AccountLogin != "" && payload.AccountID != 0 && payload.AccountType != "" {
			accountType, err := tenant.ParseAccountType(payload.AccountType)
			if err != nil {
				return err
			}
			if s.NewID == nil {
				return fmt.Errorf("missing id generator")
			}
			_, err = s.Store.UpsertTenant(ctx, tenant.Tenant{
				ID:              s.NewID(),
				Name:            payload.AccountLogin,
				AccountType:     accountType,
				GitHubAccountID: payload.AccountID,
			}, now)
			if err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

func (s *Service) Repos(ctx context.Context, tenantID string) ([]Repo, error) {
	in, err := s.Store.ActiveInstallation(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if in == nil || !in.Active {
		return nil, ErrNotInstalled
	}
	if s.GitHub == nil {
		return nil, nil
	}
	return s.GitHub.ListRepos(ctx, in.GitHubInstallationID)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}
