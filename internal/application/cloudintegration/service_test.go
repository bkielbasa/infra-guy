package cloudintegrationapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	cloudintegrationapp "infra-guy/internal/application/cloudintegration"
	"infra-guy/internal/domain/cloudintegration"
	"infra-guy/internal/domain/project"
)

type memRepo struct {
	mu           sync.Mutex
	items        map[cloudintegration.ID]*cloudintegration.Integration
	projectLinks map[project.ID][]cloudintegration.ID
}

func newMemRepo() *memRepo {
	return &memRepo{
		items:        make(map[cloudintegration.ID]*cloudintegration.Integration),
		projectLinks: make(map[project.ID][]cloudintegration.ID),
	}
}

func (m *memRepo) Save(_ context.Context, ci *cloudintegration.Integration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[ci.Snapshot().ID] = ci
	return nil
}

func (m *memRepo) Get(_ context.Context, id cloudintegration.ID) (*cloudintegration.Integration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.items[id], nil
}

func (m *memRepo) ListByTenant(_ context.Context, tenantID string) ([]*cloudintegration.Integration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*cloudintegration.Integration
	for _, ci := range m.items {
		if ci.Snapshot().TenantID == tenantID {
			out = append(out, ci)
		}
	}
	return out, nil
}

func (m *memRepo) Delete(_ context.Context, id cloudintegration.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, id)
	return nil
}

func (m *memRepo) SetProjectIntegrations(_ context.Context, projectID project.ID, ids []cloudintegration.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.projectLinks[projectID] = append([]cloudintegration.ID(nil), ids...)
	return nil
}

func (m *memRepo) ListByProject(_ context.Context, projectID project.ID) ([]*cloudintegration.Integration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*cloudintegration.Integration
	for _, id := range m.projectLinks[projectID] {
		if ci, ok := m.items[id]; ok {
			out = append(out, ci)
		}
	}
	return out, nil
}

func TestOIDCDiscoveryAndJWKS(t *testing.T) {
	repo := newMemRepo()
	svc := &cloudintegrationapp.Service{
		Integrations:  repo,
		PublicBaseURL: "https://infra-guy.example.com",
	}

	disc := svc.DiscoveryDocument()
	if disc["issuer"] != "https://infra-guy.example.com" {
		t.Fatalf("unexpected issuer: %v", disc["issuer"])
	}
	if disc["jwks_uri"] != "https://infra-guy.example.com/.well-known/jwks.json" {
		t.Fatalf("unexpected jwks_uri: %v", disc["jwks_uri"])
	}

	jwks := svc.JWKS()
	keys, ok := jwks["keys"].([]map[string]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("expected 1 key in JWKS, got %#v", jwks["keys"])
	}
	if keys[0]["alg"] != "RS256" || keys[0]["kty"] != "RSA" {
		t.Fatalf("unexpected key metadata: %#v", keys[0])
	}

	token, err := svc.MintOIDCToken(cloudintegrationapp.WorkloadClaims{
		TenantID:    "tenant-1",
		ProjectID:   "proj-1",
		ProjectName: "prod-vpc",
		RunID:       "run-123",
		Branch:      "main",
		SHA:         "deadbeef",
		Trigger:     "manual",
		Audience:    "sts.amazonaws.com",
		Duration:    15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("MintOIDCToken failed: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
}

func TestResolveCredentialsForRun_AWSOIDCAndTagMatching(t *testing.T) {
	stsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		action := r.FormValue("Action")
		if action != "AssumeRoleWithWebIdentity" {
			t.Errorf("expected Action=AssumeRoleWithWebIdentity, got %q", action)
		}
		if r.FormValue("RoleArn") != "arn:aws:iam::123456789012:role/infra-guy-prod" {
			t.Errorf("unexpected RoleArn: %q", r.FormValue("RoleArn"))
		}
		if r.FormValue("WebIdentityToken") == "" {
			t.Errorf("expected non-empty WebIdentityToken")
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = fmt.Fprint(w, `<AssumeRoleWithWebIdentityResponse>
  <AssumeRoleWithWebIdentityResult>
    <Credentials>
      <AccessKeyId>ASIAIOSFODNN7EXAMPLE</AccessKeyId>
      <SecretAccessKey>wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY</SecretAccessKey>
      <SessionToken>AQoDYXdzEJr1EXAMPLETOKEN</SessionToken>
      <Expiration>2026-10-25T23:59:59Z</Expiration>
    </Credentials>
  </AssumeRoleWithWebIdentityResult>
</AssumeRoleWithWebIdentityResponse>`)
	}))
	defer stsServer.Close()

	repo := newMemRepo()
	seq := 0
	svc := &cloudintegrationapp.Service{
		Integrations:  repo,
		PublicBaseURL: "https://infra-guy.example.com",
		STSEndpoint:   stsServer.URL,
		NewID: func() string {
			seq++
			return fmt.Sprintf("ci-%d", seq)
		},
	}

	ctx := context.Background()
	_, err := svc.Create(ctx, cloudintegration.Snapshot{
		TenantID:               "t1",
		Name:                   "aws-prod-oidc",
		Provider:               cloudintegration.ProviderAWS,
		AuthMode:               cloudintegration.AuthModeOIDC,
		RoleARN:                "arn:aws:iam::123456789012:role/infra-guy-prod",
		Region:                 "eu-central-1",
		SessionDurationSeconds: 1800,
		Tags:                   map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	projSnap := project.Snapshot{
		ID:            "p1",
		TenantID:      "t1",
		Name:          "networking",
		EffectiveTags: map[string]string{"env": "prod", "team": "platform"},
	}

	creds, err := svc.ResolveCredentialsForRun(ctx, projSnap, "r1", "main", "abc1234", "manual", "/tmp/work")
	if err != nil {
		t.Fatalf("ResolveCredentialsForRun failed: %v", err)
	}
	if creds.Env["AWS_ACCESS_KEY_ID"] != "ASIAIOSFODNN7EXAMPLE" {
		t.Fatalf("unexpected AWS_ACCESS_KEY_ID: %q", creds.Env["AWS_ACCESS_KEY_ID"])
	}
	if creds.Env["AWS_SECRET_ACCESS_KEY"] != "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" {
		t.Fatalf("unexpected AWS_SECRET_ACCESS_KEY: %q", creds.Env["AWS_SECRET_ACCESS_KEY"])
	}
	if creds.Env["AWS_SESSION_TOKEN"] != "AQoDYXdzEJr1EXAMPLETOKEN" {
		t.Fatalf("unexpected AWS_SESSION_TOKEN: %q", creds.Env["AWS_SESSION_TOKEN"])
	}
	if creds.Env["AWS_DEFAULT_REGION"] != "eu-central-1" {
		t.Fatalf("unexpected AWS_DEFAULT_REGION: %q", creds.Env["AWS_DEFAULT_REGION"])
	}
	if len(creds.RedactSecrets) < 2 {
		t.Fatalf("expected secrets marked for redaction, got %v", creds.RedactSecrets)
	}
}

func TestResolveCredentialsForRun_GCPAndAzureOIDC(t *testing.T) {
	repo := newMemRepo()
	seq := 0
	svc := &cloudintegrationapp.Service{
		Integrations:  repo,
		PublicBaseURL: "https://infra-guy.example.com",
		NewID: func() string {
			seq++
			return fmt.Sprintf("ci-%d", seq)
		},
	}
	ctx := context.Background()

	gcpCI, err := svc.Create(ctx, cloudintegration.Snapshot{
		TenantID:                    "t1",
		Name:                        "gcp-wif",
		Provider:                    cloudintegration.ProviderGCP,
		AuthMode:                    cloudintegration.AuthModeOIDC,
		GCPWorkloadIdentityProvider: "projects/123456/locations/global/workloadIdentityPools/infra/providers/oidc",
		GCPServiceAccountEmail:      "terraform@my-gcp-project.iam.gserviceaccount.com",
	})
	if err != nil {
		t.Fatalf("Create GCP failed: %v", err)
	}

	azCI, err := svc.Create(ctx, cloudintegration.Snapshot{
		TenantID:            "t1",
		Name:                "azure-oidc",
		Provider:            cloudintegration.ProviderAzure,
		AuthMode:            cloudintegration.AuthModeOIDC,
		AzureTenantID:       "00000000-0000-0000-0000-000000000001",
		AzureClientID:       "00000000-0000-0000-0000-000000000002",
		AzureSubscriptionID: "00000000-0000-0000-0000-000000000003",
	})
	if err != nil {
		t.Fatalf("Create Azure failed: %v", err)
	}

	err = svc.SetProjectIntegrations(ctx, "t1", "p1", []cloudintegration.ID{gcpCI.Snapshot().ID, azCI.Snapshot().ID})
	if err != nil {
		t.Fatalf("SetProjectIntegrations failed: %v", err)
	}

	projSnap := project.Snapshot{
		ID:       "p1",
		TenantID: "t1",
		Name:     "multicloud",
	}
	creds, err := svc.ResolveCredentialsForRun(ctx, projSnap, "r2", "main", "def5678", "manual", "/tmp/work")
	if err != nil {
		t.Fatalf("ResolveCredentialsForRun failed: %v", err)
	}

	credJSON, ok := creds.Files[".infraguy/gcp_external_account.json"]
	if !ok {
		t.Fatalf("expected .infraguy/gcp_external_account.json file, got %v", creds.Files)
	}
	var parsed map[string]any
	if err := json.Unmarshal(credJSON, &parsed); err != nil {
		t.Fatalf("invalid gcp_external_account.json: %v", err)
	}
	if parsed["type"] != "external_account" {
		t.Fatalf("expected external_account, got %v", parsed["type"])
	}

	if creds.Env["ARM_USE_OIDC"] != "true" || creds.Env["ARM_CLIENT_ID"] != "00000000-0000-0000-0000-000000000002" {
		t.Fatalf("unexpected Azure OIDC env: %#v", creds.Env)
	}
}
