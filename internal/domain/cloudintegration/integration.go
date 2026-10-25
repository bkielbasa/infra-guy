package cloudintegration

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type ID string

const (
	ProviderAWS   = "aws"
	ProviderGCP   = "gcp"
	ProviderAzure = "azure"

	AuthModeOIDC       = "oidc"
	AuthModeAssumeRole = "assume_role"

	DefaultAWSRegion       = "us-east-1"
	DefaultAWSAudience     = "sts.amazonaws.com"
	DefaultSessionDuration = 3600
)

var (
	ErrInvalidName     = errors.New("cloud integration name is required")
	ErrInvalidProvider = errors.New("unsupported cloud provider")
	ErrInvalidRoleARN  = errors.New("AWS IAM Role ARN is required")
)

type Snapshot struct {
	ID                          ID
	TenantID                    string
	Name                        string
	Provider                    string
	AuthMode                    string
	RoleARN                     string
	Region                      string
	Audience                    string
	SessionDurationSeconds      int
	ExternalID                  string
	AccessKeyID                 string
	SecretAccessKey             string
	SecretAccessKeyCiphertext   []byte
	GCPWorkloadIdentityProvider string
	GCPServiceAccountEmail      string
	AzureClientID               string
	AzureTenantID               string
	AzureSubscriptionID         string
	Tags                        map[string]string
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
}

type Integration struct {
	s Snapshot
}

func New(in Snapshot, now time.Time) (*Integration, error) {
	if strings.TrimSpace(string(in.ID)) == "" || strings.TrimSpace(in.TenantID) == "" {
		return nil, errors.New("cloud integration requires id and tenant_id")
	}
	in.CreatedAt = now.UTC()
	in.UpdatedAt = now.UTC()
	c := &Integration{s: in}
	if err := c.normalizeAndValidate(); err != nil {
		return nil, err
	}
	return c, nil
}

func Restore(s Snapshot) *Integration {
	s.Tags = cloneTags(s.Tags)
	if s.Provider == "" {
		s.Provider = ProviderAWS
	}
	if s.AuthMode == "" {
		s.AuthMode = AuthModeOIDC
	}
	if s.Region == "" {
		s.Region = DefaultAWSRegion
	}
	if s.Audience == "" {
		s.Audience = DefaultAWSAudience
	}
	if s.SessionDurationSeconds <= 0 {
		s.SessionDurationSeconds = DefaultSessionDuration
	}
	return &Integration{s: s}
}

func (c *Integration) Snapshot() Snapshot {
	out := c.s
	out.Tags = cloneTags(c.s.Tags)
	out.SecretAccessKeyCiphertext = append([]byte(nil), c.s.SecretAccessKeyCiphertext...)
	return out
}

func (c *Integration) Update(in Snapshot, now time.Time) error {
	c.s.Name = in.Name
	c.s.Provider = in.Provider
	c.s.AuthMode = in.AuthMode
	c.s.RoleARN = in.RoleARN
	c.s.Region = in.Region
	c.s.Audience = in.Audience
	c.s.SessionDurationSeconds = in.SessionDurationSeconds
	c.s.ExternalID = in.ExternalID
	c.s.AccessKeyID = in.AccessKeyID
	if in.SecretAccessKey != "" {
		c.s.SecretAccessKey = in.SecretAccessKey
	}
	if len(in.SecretAccessKeyCiphertext) > 0 {
		c.s.SecretAccessKeyCiphertext = append([]byte(nil), in.SecretAccessKeyCiphertext...)
	}
	c.s.GCPWorkloadIdentityProvider = in.GCPWorkloadIdentityProvider
	c.s.GCPServiceAccountEmail = in.GCPServiceAccountEmail
	c.s.AzureClientID = in.AzureClientID
	c.s.AzureTenantID = in.AzureTenantID
	c.s.AzureSubscriptionID = in.AzureSubscriptionID
	c.s.Tags = cloneTags(in.Tags)
	c.s.UpdatedAt = now.UTC()
	return c.normalizeAndValidate()
}

func (c *Integration) SetCiphertext(ct []byte) {
	c.s.SecretAccessKeyCiphertext = append([]byte(nil), ct...)
	c.s.SecretAccessKey = ""
}

func (c *Integration) normalizeAndValidate() error {
	c.s.Name = strings.TrimSpace(c.s.Name)
	if c.s.Name == "" {
		return ErrInvalidName
	}
	c.s.Provider = strings.ToLower(strings.TrimSpace(c.s.Provider))
	if c.s.Provider == "" {
		c.s.Provider = ProviderAWS
	}
	switch c.s.Provider {
	case ProviderAWS, ProviderGCP, ProviderAzure:
	default:
		return fmt.Errorf("%w: %s", ErrInvalidProvider, c.s.Provider)
	}
	c.s.AuthMode = strings.ToLower(strings.TrimSpace(c.s.AuthMode))
	if c.s.AuthMode == "" {
		c.s.AuthMode = AuthModeOIDC
	}
	if c.s.AuthMode != AuthModeOIDC && c.s.AuthMode != AuthModeAssumeRole {
		c.s.AuthMode = AuthModeOIDC
	}
	c.s.RoleARN = strings.TrimSpace(c.s.RoleARN)
	c.s.Region = strings.TrimSpace(c.s.Region)
	if c.s.Region == "" {
		c.s.Region = DefaultAWSRegion
	}
	c.s.Audience = strings.TrimSpace(c.s.Audience)
	if c.s.Audience == "" {
		switch c.s.Provider {
		case ProviderAzure:
			c.s.Audience = "api://AzureADTokenExchange"
		default:
			c.s.Audience = DefaultAWSAudience
		}
	}
	if c.s.SessionDurationSeconds <= 0 {
		c.s.SessionDurationSeconds = DefaultSessionDuration
	}
	if c.s.SessionDurationSeconds < 900 {
		c.s.SessionDurationSeconds = 900
	}
	if c.s.SessionDurationSeconds > 43200 {
		c.s.SessionDurationSeconds = 43200
	}
	c.s.ExternalID = strings.TrimSpace(c.s.ExternalID)
	c.s.AccessKeyID = strings.TrimSpace(c.s.AccessKeyID)
	c.s.GCPWorkloadIdentityProvider = strings.TrimSpace(c.s.GCPWorkloadIdentityProvider)
	c.s.GCPServiceAccountEmail = strings.TrimSpace(c.s.GCPServiceAccountEmail)
	c.s.AzureClientID = strings.TrimSpace(c.s.AzureClientID)
	c.s.AzureTenantID = strings.TrimSpace(c.s.AzureTenantID)
	c.s.AzureSubscriptionID = strings.TrimSpace(c.s.AzureSubscriptionID)

	if c.s.Provider == ProviderAWS && c.s.RoleARN == "" {
		return ErrInvalidRoleARN
	}
	return nil
}

// MatchesProjectTags checks if this Cloud Integration should automatically bind
// to a workspace based on its tags (either global scope or matching key:value tag).
func (c *Integration) MatchesProjectTags(projectTags map[string]string) (bool, string) {
	if c == nil || len(c.s.Tags) == 0 {
		return false, ""
	}
	for _, k := range []string{"scope", "enforcement"} {
		if strings.EqualFold(c.s.Tags[k], "global") {
			return true, k + ":global"
		}
	}
	if strings.EqualFold(c.s.Tags["global"], "true") {
		return true, "global:true"
	}
	if len(projectTags) == 0 {
		return false, ""
	}
	keys := make([]string, 0, len(c.s.Tags))
	for k := range c.s.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := c.s.Tags[k]
		if pv, ok := projectTags[k]; ok && pv == v {
			return true, k + ":" + v
		}
	}
	return false, ""
}

func cloneTags(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
