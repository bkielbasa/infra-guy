package tenant

import "fmt"

type ID string

type Tenant struct {
	ID              string
	Name            string
	AccountType     string
	GitHubAccountID int64
}

type Installation struct {
	ID                   string
	TenantID             string
	AccountLogin         string
	AccountType          string
	GitHubInstallationID int64
	Active               bool
}

type Membership struct {
	TenantID   string
	TenantName string
	UserID     string
	Role       string
}

func ParseAccountType(githubType string) (string, error) {
	switch githubType {
	case "User":
		return "user", nil
	case "Organization":
		return "organization", nil
	default:
		return "", fmt.Errorf("unsupported github account type %q", githubType)
	}
}
