package cloudintegrationapp

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"infra-guy/internal/domain/cloudintegration"
	"infra-guy/internal/domain/project"
)

var ErrNotFound = errors.New("cloud integration not found")

type SecretSealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
}

type AADSecretSealer interface {
	SecretSealer
	SealWithAAD(plaintext, aad []byte) ([]byte, error)
	OpenWithAAD(ciphertext, aad []byte) ([]byte, error)
}

// IntegrationAAD constructs the deterministic AEAD Associated Data for a cloud integration's secret key.
func IntegrationAAD(id cloudintegration.ID) []byte {
	return []byte("cloud_integration:" + string(id) + ":secret_access_key")
}

type ResolvedIntegration struct {
	Integration *cloudintegration.Integration
	Source      string // "workspace", "tag", "global"
	MatchedTag  string
}

type WorkloadClaims struct {
	TenantID    string
	ProjectID   string
	ProjectName string
	RunID       string
	Branch      string
	SHA         string
	Trigger     string
	Audience    string
	Duration    time.Duration
}

type RunCredentials struct {
	Env           map[string]string
	Files         map[string][]byte
	RedactSecrets []string
	Summary       []string
}

type Service struct {
	Integrations  cloudintegration.Repository
	Sealer        SecretSealer
	PublicBaseURL string
	PrivateKey    *rsa.PrivateKey
	STSEndpoint   string // Optional override for testing; defaults to https://sts.<region>.amazonaws.com
	HTTPClient    *http.Client
	Now           func() time.Time
	NewID         func() string

	keyOnce      sync.Once
	generatedKey *rsa.PrivateKey
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) httpClient() *http.Client {
	if s != nil && s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (s *Service) Issuer() string {
	base := ""
	if s != nil {
		base = strings.TrimRight(strings.TrimSpace(s.PublicBaseURL), "/")
	}
	if base == "" {
		return "https://infra-guy.local"
	}
	return base
}

func (s *Service) signingKey() *rsa.PrivateKey {
	if s != nil && s.PrivateKey != nil {
		return s.PrivateKey
	}
	s.keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err == nil {
			s.generatedKey = k
		}
	})
	return s.generatedKey
}

func (s *Service) KeyID() string {
	priv := s.signingKey()
	if priv == nil {
		return "infraguy-oidc-1"
	}
	sum := sha256.Sum256(priv.PublicKey.N.Bytes())
	return hex.EncodeToString(sum[:8])
}

// DiscoveryDocument returns the OpenID Connect Discovery 1.0 metadata JSON.
func (s *Service) DiscoveryDocument() map[string]any {
	iss := s.Issuer()
	return map[string]any{
		"issuer":                                iss,
		"jwks_uri":                              iss + "/.well-known/jwks.json",
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"claims_supported": []string{
			"sub", "aud", "iss", "iat", "nbf", "exp", "jti",
			"tenant_id", "project_id", "project_name", "run_id", "branch", "commit_sha", "trigger",
		},
	}
}

// JWKS returns the RFC 7517 JSON Web Key Set containing the OIDC public key.
func (s *Service) JWKS() map[string]any {
	priv := s.signingKey()
	if priv == nil {
		return map[string]any{"keys": []any{}}
	}
	pub := priv.PublicKey
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	e := base64.RawURLEncoding.EncodeToString(eBytes)
	return map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": s.KeyID(),
				"n":   n,
				"e":   e,
			},
		},
	}
}

// MintOIDCToken signs a workload identity JWT for cloud provider OIDC federation.
func (s *Service) MintOIDCToken(claims WorkloadClaims) (string, error) {
	priv := s.signingKey()
	if priv == nil {
		return "", errors.New("no OIDC signing key available")
	}
	now := s.now()
	dur := claims.Duration
	if dur <= 0 {
		dur = time.Hour
	}
	aud := strings.TrimSpace(claims.Audience)
	if aud == "" {
		aud = cloudintegration.DefaultAWSAudience
	}
	branch := claims.Branch
	if branch == "" {
		branch = "main"
	}
	trigger := claims.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	sub := fmt.Sprintf("org:%s:project:%s:branch:%s:trigger:%s", claims.TenantID, claims.ProjectID, branch, trigger)
	jti := claims.RunID
	if jti == "" && s.NewID != nil {
		jti = s.NewID()
	}

	header := map[string]any{
		"alg": "RS256",
		"typ": "JWT",
		"kid": s.KeyID(),
	}
	payload := map[string]any{
		"iss":          s.Issuer(),
		"sub":          sub,
		"aud":          aud,
		"iat":          now.Unix(),
		"nbf":          now.Unix(),
		"exp":          now.Add(dur).Unix(),
		"jti":          jti,
		"tenant_id":    claims.TenantID,
		"project_id":   claims.ProjectID,
		"project_name": claims.ProjectName,
		"run_id":       claims.RunID,
		"branch":       branch,
		"commit_sha":   claims.SHA,
		"trigger":      trigger,
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (s *Service) List(ctx context.Context, tenantID string) ([]*cloudintegration.Integration, error) {
	if s == nil || s.Integrations == nil {
		return nil, nil
	}
	return s.Integrations.ListByTenant(ctx, tenantID)
}

func (s *Service) Get(ctx context.Context, tenantID string, id cloudintegration.ID) (*cloudintegration.Integration, error) {
	if s == nil || s.Integrations == nil {
		return nil, nil
	}
	c, err := s.Integrations.Get(ctx, id)
	if err != nil || c == nil {
		return nil, err
	}
	if c.Snapshot().TenantID != tenantID {
		return nil, nil
	}
	return c, nil
}

func (s *Service) Create(ctx context.Context, in cloudintegration.Snapshot) (*cloudintegration.Integration, error) {
	if in.ID == "" && s.NewID != nil {
		in.ID = cloudintegration.ID(s.NewID())
	}
	c, err := cloudintegration.New(in, s.now())
	if err != nil {
		return nil, err
	}
	if in.SecretAccessKey != "" && s.Sealer != nil {
		var ct []byte
		if aadSealer, ok := s.Sealer.(AADSecretSealer); ok {
			ct, err = aadSealer.SealWithAAD([]byte(in.SecretAccessKey), IntegrationAAD(c.Snapshot().ID))
		} else {
			ct, err = s.Sealer.Seal([]byte(in.SecretAccessKey))
		}
		if err != nil {
			return nil, fmt.Errorf("seal secret access key: %w", err)
		}
		c.SetCiphertext(ct)
	}
	if err := s.Integrations.Save(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Service) Update(ctx context.Context, tenantID string, id cloudintegration.ID, in cloudintegration.Snapshot) (*cloudintegration.Integration, error) {
	c, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound
	}
	if err := c.Update(in, s.now()); err != nil {
		return nil, err
	}
	if in.SecretAccessKey != "" && s.Sealer != nil {
		var ct []byte
		if aadSealer, ok := s.Sealer.(AADSecretSealer); ok {
			ct, err = aadSealer.SealWithAAD([]byte(in.SecretAccessKey), IntegrationAAD(id))
		} else {
			ct, err = s.Sealer.Seal([]byte(in.SecretAccessKey))
		}
		if err != nil {
			return nil, fmt.Errorf("seal secret access key: %w", err)
		}
		c.SetCiphertext(ct)
	}
	if err := s.Integrations.Save(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Service) Delete(ctx context.Context, tenantID string, id cloudintegration.ID) error {
	c, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound
	}
	return s.Integrations.Delete(ctx, id)
}

func (s *Service) SetProjectIntegrations(ctx context.Context, tenantID string, projectID project.ID, ids []cloudintegration.ID) error {
	if s == nil || s.Integrations == nil {
		return nil
	}
	seen := make(map[cloudintegration.ID]bool, len(ids))
	var deduped []cloudintegration.ID
	for _, id := range ids {
		id = cloudintegration.ID(strings.TrimSpace(string(id)))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		c, err := s.Get(ctx, tenantID, id)
		if err != nil {
			return err
		}
		if c == nil {
			return fmt.Errorf("cloud integration %s not found", id)
		}
		deduped = append(deduped, id)
	}
	return s.Integrations.SetProjectIntegrations(ctx, projectID, deduped)
}

func (s *Service) ListByProject(ctx context.Context, projectID project.ID) ([]*cloudintegration.Integration, error) {
	if s == nil || s.Integrations == nil {
		return nil, nil
	}
	return s.Integrations.ListByProject(ctx, projectID)
}

// ResolveForProject returns explicitly attached Cloud Integrations plus any
// tenant Cloud Integrations whose tags match the workspace's tags.
func (s *Service) ResolveForProject(ctx context.Context, tenantID string, projectID project.ID, projectTags map[string]string) ([]ResolvedIntegration, error) {
	if s == nil || s.Integrations == nil {
		return nil, nil
	}
	seen := make(map[cloudintegration.ID]bool)
	var out []ResolvedIntegration

	explicit, err := s.Integrations.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, c := range explicit {
		if c == nil {
			continue
		}
		id := c.Snapshot().ID
		seen[id] = true
		out = append(out, ResolvedIntegration{
			Integration: c,
			Source:      "workspace",
		})
	}

	if tenantID != "" {
		all, err := s.Integrations.ListByTenant(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		sort.Slice(all, func(i, j int) bool {
			return all[i].Snapshot().Name < all[j].Snapshot().Name
		})
		for _, c := range all {
			if c == nil || seen[c.Snapshot().ID] {
				continue
			}
			if ok, matched := c.MatchesProjectTags(projectTags); ok {
				seen[c.Snapshot().ID] = true
				src := "tag"
				if strings.HasSuffix(matched, ":global") || matched == "global:true" {
					src = "global"
				}
				out = append(out, ResolvedIntegration{
					Integration: c,
					Source:      src,
					MatchedTag:  matched,
				})
			}
		}
	}
	return out, nil
}

// ResolveCredentialsForRun dynamically generates short-lived credentials and OIDC
// workload tokens for all Cloud Integrations bound to the workspace.
func (s *Service) ResolveCredentialsForRun(ctx context.Context, p project.Snapshot, runID, branch, sha, trigger, homeDir string) (RunCredentials, error) {
	res := RunCredentials{
		Env:   map[string]string{},
		Files: map[string][]byte{},
	}
	if s == nil || s.Integrations == nil {
		return res, nil
	}
	resolved, err := s.ResolveForProject(ctx, p.TenantID, p.ID, p.Tags)
	if err != nil {
		return res, err
	}
	if len(resolved) == 0 {
		return res, nil
	}

	for _, item := range resolved {
		snap := item.Integration.Snapshot()
		dur := time.Duration(snap.SessionDurationSeconds) * time.Second
		if dur <= 0 {
			dur = time.Hour
		}
		claims := WorkloadClaims{
			TenantID:    p.TenantID,
			ProjectID:   string(p.ID),
			ProjectName: p.Name,
			RunID:       runID,
			Branch:      branch,
			SHA:         sha,
			Trigger:     trigger,
			Audience:    snap.Audience,
			Duration:    dur,
		}
		oidcToken, err := s.MintOIDCToken(claims)
		if err != nil {
			return res, fmt.Errorf("mint OIDC token for %q: %w", snap.Name, err)
		}
		res.Env["INFRAGUY_OIDC_TOKEN"] = oidcToken
		res.RedactSecrets = append(res.RedactSecrets, oidcToken)

		switch snap.Provider {
		case cloudintegration.ProviderAWS:
			if err := s.resolveAWS(ctx, snap, oidcToken, runID, homeDir, &res); err != nil {
				return res, err
			}
		case cloudintegration.ProviderGCP:
			s.resolveGCP(snap, oidcToken, homeDir, &res)
		case cloudintegration.ProviderAzure:
			s.resolveAzure(snap, oidcToken, homeDir, &res)
		}
	}
	return res, nil
}

type stsAssumeRoleResponse struct {
	Credentials stsCredentials `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
}

type stsAssumeRoleDirectResponse struct {
	Credentials stsCredentials `xml:"AssumeRoleResult>Credentials"`
}

type stsCredentials struct {
	AccessKeyId     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

func (s *Service) resolveAWS(ctx context.Context, snap cloudintegration.Snapshot, oidcToken, runID, homeDir string, out *RunCredentials) error {
	region := snap.Region
	if region == "" {
		region = cloudintegration.DefaultAWSRegion
	}
	out.Env["AWS_REGION"] = region
	out.Env["AWS_DEFAULT_REGION"] = region
	out.Env["AWS_ROLE_ARN"] = snap.RoleARN

	sessionName := "infraguy-" + sanitizeSessionName(runID)
	if sessionName == "infraguy-" {
		sessionName = "infraguy-session"
	}
	out.Env["AWS_ROLE_SESSION_NAME"] = sessionName

	tokenRelPath := ".infraguy/aws_oidc_token"
	out.Files[tokenRelPath] = []byte(oidcToken)
	if homeDir != "" {
		out.Env["AWS_WEB_IDENTITY_TOKEN_FILE"] = filepath.Join(homeDir, filepath.FromSlash(tokenRelPath))
	}

	if snap.AuthMode == cloudintegration.AuthModeAssumeRole {
		secretKey := snap.SecretAccessKey
		if secretKey == "" && len(snap.SecretAccessKeyCiphertext) > 0 && s.Sealer != nil {
			var pt []byte
			var err error
			if aadSealer, ok := s.Sealer.(AADSecretSealer); ok {
				pt, err = aadSealer.OpenWithAAD(snap.SecretAccessKeyCiphertext, IntegrationAAD(snap.ID))
			} else {
				pt, err = s.Sealer.Open(snap.SecretAccessKeyCiphertext)
			}
			if err != nil {
				return fmt.Errorf("decrypt AWS secret key for %q: %w", snap.Name, err)
			}
			secretKey = string(pt)
		}
		if snap.AccessKeyID != "" && secretKey != "" {
			creds, err := s.assumeRoleSigV4(ctx, region, snap.RoleARN, sessionName, snap.ExternalID, snap.SessionDurationSeconds, snap.AccessKeyID, secretKey)
			if err != nil {
				return fmt.Errorf("AWS STS AssumeRole for %q: %w", snap.Name, err)
			}
			out.Env["AWS_ACCESS_KEY_ID"] = creds.AccessKeyId
			out.Env["AWS_SECRET_ACCESS_KEY"] = creds.SecretAccessKey
			out.Env["AWS_SESSION_TOKEN"] = creds.SessionToken
			out.RedactSecrets = append(out.RedactSecrets, creds.SecretAccessKey, creds.SessionToken)
			out.Summary = append(out.Summary, fmt.Sprintf("AWS (%s): minted short-lived STS credentials for %s", snap.Name, snap.RoleARN))
			return nil
		}
	}

	// AuthModeOIDC: attempt STS AssumeRoleWithWebIdentity exchange if STS endpoint is reachable,
	// while also keeping AWS_WEB_IDENTITY_TOKEN_FILE configured for native Terraform AWS provider support.
	creds, err := s.assumeRoleWithWebIdentity(ctx, region, snap.RoleARN, sessionName, oidcToken, snap.SessionDurationSeconds)
	if err == nil && creds.AccessKeyId != "" {
		out.Env["AWS_ACCESS_KEY_ID"] = creds.AccessKeyId
		out.Env["AWS_SECRET_ACCESS_KEY"] = creds.SecretAccessKey
		out.Env["AWS_SESSION_TOKEN"] = creds.SessionToken
		out.RedactSecrets = append(out.RedactSecrets, creds.SecretAccessKey, creds.SessionToken)
		out.Summary = append(out.Summary, fmt.Sprintf("AWS OIDC (%s): exchanged WebIdentity token for temporary STS credentials (%s)", snap.Name, snap.RoleARN))
	} else {
		out.Summary = append(out.Summary, fmt.Sprintf("AWS OIDC (%s): provisioned OIDC WebIdentity token for %s", snap.Name, snap.RoleARN))
	}
	return nil
}

func (s *Service) stsBaseURL(region string) string {
	if s != nil && strings.TrimSpace(s.STSEndpoint) != "" {
		return strings.TrimRight(strings.TrimSpace(s.STSEndpoint), "/")
	}
	return fmt.Sprintf("https://sts.%s.amazonaws.com", region)
}

func (s *Service) assumeRoleWithWebIdentity(ctx context.Context, region, roleARN, sessionName, webIdentityToken string, durationSeconds int) (stsCredentials, error) {
	q := url.Values{}
	q.Set("Action", "AssumeRoleWithWebIdentity")
	q.Set("Version", "2011-06-15")
	q.Set("RoleArn", roleARN)
	q.Set("RoleSessionName", sessionName)
	q.Set("WebIdentityToken", webIdentityToken)
	if durationSeconds >= 900 {
		q.Set("DurationSeconds", strconv.Itoa(durationSeconds))
	}
	endpoint := s.stsBaseURL(region) + "/?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return stsCredentials{}, err
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return stsCredentials{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return stsCredentials{}, fmt.Errorf("sts status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed stsAssumeRoleResponse
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return stsCredentials{}, err
	}
	return parsed.Credentials, nil
}

func (s *Service) assumeRoleSigV4(ctx context.Context, region, roleARN, sessionName, externalID string, durationSeconds int, accessKeyID, secretAccessKey string) (stsCredentials, error) {
	q := url.Values{}
	q.Set("Action", "AssumeRole")
	q.Set("Version", "2011-06-15")
	q.Set("RoleArn", roleARN)
	q.Set("RoleSessionName", sessionName)
	if externalID != "" {
		q.Set("ExternalId", externalID)
	}
	if durationSeconds >= 900 {
		q.Set("DurationSeconds", strconv.Itoa(durationSeconds))
	}
	bodyStr := q.Encode()
	baseURL := s.stsBaseURL(region)
	u, err := url.Parse(baseURL + "/")
	if err != nil {
		return stsCredentials{}, err
	}
	now := s.now()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := sha256Hex([]byte(bodyStr))
	canonicalHeaders := fmt.Sprintf("content-type:application/x-www-form-urlencoded\nhost:%s\nx-amz-date:%s\n", u.Host, amzDate)
	signedHeaders := "content-type;host;x-amz-date"
	canonicalRequest := strings.Join([]string{
		"POST",
		u.Path,
		"",
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/sts/aws4_request", dateStamp, region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretAccessKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "sts")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	authHeader := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKeyID, credentialScope, signedHeaders, signature,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(bodyStr))
	if err != nil {
		return stsCredentials{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Authorization", authHeader)

	resp, err := s.httpClient().Do(req)
	if err != nil {
		return stsCredentials{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return stsCredentials{}, fmt.Errorf("sts status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var parsed stsAssumeRoleDirectResponse
	if err := xml.Unmarshal(respBody, &parsed); err != nil {
		return stsCredentials{}, err
	}
	return parsed.Credentials, nil
}

func (s *Service) resolveGCP(snap cloudintegration.Snapshot, oidcToken, homeDir string, out *RunCredentials) {
	tokenRel := ".infraguy/gcp_oidc_token"
	credRel := ".infraguy/gcp_external_account.json"
	out.Files[tokenRel] = []byte(oidcToken)

	tokenFilePath := tokenRel
	credFilePath := credRel
	if homeDir != "" {
		tokenFilePath = filepath.Join(homeDir, filepath.FromSlash(tokenRel))
		credFilePath = filepath.Join(homeDir, filepath.FromSlash(credRel))
	}

	aud := snap.GCPWorkloadIdentityProvider
	if !strings.HasPrefix(aud, "//iam.googleapis.com/") && aud != "" {
		aud = "//iam.googleapis.com/" + strings.TrimPrefix(aud, "/")
	}
	doc := map[string]any{
		"type":               "external_account",
		"audience":           aud,
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"token_url":          "https://sts.googleapis.com/v1/token",
		"credential_source": map[string]any{
			"file": tokenFilePath,
		},
	}
	if snap.GCPServiceAccountEmail != "" {
		doc["service_account_impersonation_url"] = fmt.Sprintf(
			"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken",
			snap.GCPServiceAccountEmail,
		)
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	out.Files[credRel] = b
	out.Env["GOOGLE_APPLICATION_CREDENTIALS"] = credFilePath
	out.Summary = append(out.Summary, fmt.Sprintf("GCP OIDC (%s): configured Workload Identity Federation (%s)", snap.Name, snap.GCPWorkloadIdentityProvider))
}

func (s *Service) resolveAzure(snap cloudintegration.Snapshot, oidcToken, homeDir string, out *RunCredentials) {
	tokenRel := ".infraguy/azure_oidc_token"
	out.Files[tokenRel] = []byte(oidcToken)
	out.Env["ARM_USE_OIDC"] = "true"
	out.Env["ARM_OIDC_TOKEN"] = oidcToken
	if homeDir != "" {
		out.Env["ARM_OIDC_TOKEN_FILE_PATH"] = filepath.Join(homeDir, filepath.FromSlash(tokenRel))
	}
	if snap.AzureClientID != "" {
		out.Env["ARM_CLIENT_ID"] = snap.AzureClientID
	}
	if snap.AzureTenantID != "" {
		out.Env["ARM_TENANT_ID"] = snap.AzureTenantID
	}
	if snap.AzureSubscriptionID != "" {
		out.Env["ARM_SUBSCRIPTION_ID"] = snap.AzureSubscriptionID
	}
	out.Summary = append(out.Summary, fmt.Sprintf("Azure OIDC (%s): configured Workload Identity Federation (client %s)", snap.Name, snap.AzureClientID))
}

func sanitizeSessionName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(data))
	return h.Sum(nil)
}
