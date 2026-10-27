package project

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const DefaultTerraformVersion = "1.9.8"
const DefaultDriftIntervalMinutes = 360

const (
	EngineTerraform  = "terraform"
	EngineKubernetes = "kubernetes"
	EngineAnsible    = "ansible"
	EngineHelm       = "helm"
	DefaultEngine    = EngineTerraform
)

const (
	DriftStatusUnknown = "unknown"
	DriftStatusSynced  = "synced"
	DriftStatusDrifted = "drifted"
	DriftStatusError   = "error"
)

var (
	ErrBusy                    = errors.New("another run is active")
	ErrLocked                  = errors.New("project is locked")
	ErrActiveRunMismatch       = errors.New("active run does not match")
	ErrInvalidTerraformVersion = errors.New("invalid terraform version")
	ErrInvalidEngine           = errors.New("invalid engine")
	ErrInvalidTag              = errors.New("invalid tag: expected key:value")
)

type EngineConfig struct {
	// Kubernetes / Kustomize (#17)
	K8sNamespace string `json:"k8s_namespace,omitempty"`
	Kustomize    bool   `json:"kustomize,omitempty"`
	K8sPrune     bool   `json:"k8s_prune,omitempty"`
	ManifestPath string `json:"manifest_path,omitempty"`

	// Ansible (#18)
	Playbook         string `json:"playbook,omitempty"`
	Inventory        string `json:"inventory,omitempty"`
	ExtraVars        string `json:"extra_vars,omitempty"`
	RequirementsFile string `json:"requirements_file,omitempty"`
	Become           bool   `json:"become,omitempty"`

	// Helm (#19)
	ReleaseName     string `json:"release_name,omitempty"`
	HelmNamespace   string `json:"helm_namespace,omitempty"`
	Chart           string `json:"chart,omitempty"`
	RepoURL         string `json:"repo_url,omitempty"`
	ChartVersion    string `json:"chart_version,omitempty"`
	ValuesFiles     string `json:"values_files,omitempty"`
	SetValues       string `json:"set_values,omitempty"`
	Atomic          bool   `json:"atomic,omitempty"`
	Wait            bool   `json:"wait,omitempty"`
	CreateNamespace bool   `json:"create_namespace,omitempty"`

	// kubectl, Helm, or ansible-core version. Empty uses the runner PATH.
	ToolVersion string `json:"tool_version,omitempty"`
}

type ID string

type Snapshot struct {
	ID                   ID
	TenantID             string
	GitHubRepoID         int64
	Name                 string
	Repo                 SourceRepo
	DefaultBranch        string
	WorkingDirectory     string
	Engine               string
	EngineConfig         EngineConfig
	TerraformVersion     string
	ApprovalPolicyID     string
	AutoApprove          bool
	Tags                 map[string]string
	Locked               bool
	StateToken           string
	StateTokenHash       string
	ActiveRunID          string
	DriftEnabled         bool
	DriftIntervalMinutes int
	DriftStatus          string
	LastDriftCheckAt     *time.Time
	TTLHours             int
	ExpiresAt            *time.Time
	AutoDestroy          bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type NewInput struct {
	ID                   ID
	TenantID             string
	GitHubRepoID         int64
	Name                 string
	Repo                 SourceRepo
	DefaultBranch        string
	WorkingDirectory     string
	Engine               string
	EngineConfig         EngineConfig
	TerraformVersion     string
	ApprovalPolicyID     string
	AutoApprove          bool
	Tags                 map[string]string
	DriftEnabled         bool
	DriftIntervalMinutes int
	TTLHours             int
	AutoDestroy          bool
	Now                  time.Time
}

type Project struct{ s Snapshot }

func New(id ID, name string, repo SourceRepo, now time.Time) (*Project, error) {
	if id == "" || strings.TrimSpace(name) == "" || repo.Owner == "" {
		return nil, fmt.Errorf("project requires id, name, and repository")
	}
	return &Project{s: Snapshot{
		ID: id, Name: strings.TrimSpace(name), Repo: repo,
		DefaultBranch: "main", Engine: DefaultEngine, TerraformVersion: DefaultTerraformVersion,
		DriftIntervalMinutes: DefaultDriftIntervalMinutes,
		DriftStatus:          DriftStatusUnknown,
		CreatedAt:            now, UpdatedAt: now,
	}}, nil
}

func Restore(s Snapshot) *Project {
	s.ApprovalPolicyID = strings.TrimSpace(s.ApprovalPolicyID)
	s.Tags = cloneTags(s.Tags)
	if s.Engine == "" {
		s.Engine = DefaultEngine
	}
	if s.DriftIntervalMinutes <= 0 {
		s.DriftIntervalMinutes = DefaultDriftIntervalMinutes
	}
	if s.DriftStatus == "" {
		s.DriftStatus = DriftStatusUnknown
	}
	return &Project{s: s}
}

func Rehydrate(s Snapshot) *Project { return Restore(s) }

func (p *Project) Snapshot() Snapshot {
	s := p.s
	s.Tags = cloneTags(p.s.Tags)
	return s
}

func (p *Project) ApprovalPolicyID() string { return p.s.ApprovalPolicyID }

func (p *Project) ChangeApprovalPolicy(policyID string) {
	p.s.ApprovalPolicyID = strings.TrimSpace(policyID)
}

func (p *Project) SetApprovalPolicyID(policyID string) {
	p.ChangeApprovalPolicy(policyID)
}

func (p *Project) SetWorkingDirectory(raw string) error {
	dir, err := ParseWorkingDirectory(raw)
	if err != nil {
		return err
	}
	p.s.WorkingDirectory = dir
	return nil
}

func (p *Project) SetDefaultBranch(branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" || strings.Contains(branch, " ") {
		return fmt.Errorf("invalid branch")
	}
	p.s.DefaultBranch = branch
	return nil
}

func ValidateTerraformVersion(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return DefaultTerraformVersion, nil
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "", ErrInvalidTerraformVersion
	}
	for _, part := range parts {
		if part == "" {
			return "", ErrInvalidTerraformVersion
		}
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return "", ErrInvalidTerraformVersion
			}
		}
	}
	return v, nil
}

func (p *Project) SetTerraformVersion(raw string) error {
	v, err := ValidateTerraformVersion(raw)
	if err != nil {
		return err
	}
	p.s.TerraformVersion = v
	return nil
}

func ValidateEngine(raw string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return DefaultEngine, nil
	}
	switch v {
	case EngineTerraform, EngineKubernetes, EngineAnsible, EngineHelm:
		return v, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidEngine, raw)
	}
}

func EngineLabel(engine string) string {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case EngineKubernetes:
		return "Kubernetes"
	case EngineAnsible:
		return "Ansible"
	case EngineHelm:
		return "Helm"
	default:
		return "Terraform"
	}
}

func CleanEngineConfig(cfg EngineConfig) EngineConfig {
	return EngineConfig{
		K8sNamespace:     strings.TrimSpace(cfg.K8sNamespace),
		Kustomize:        cfg.Kustomize,
		K8sPrune:         cfg.K8sPrune,
		ManifestPath:     strings.TrimSpace(cfg.ManifestPath),
		Playbook:         strings.TrimSpace(cfg.Playbook),
		Inventory:        strings.TrimSpace(cfg.Inventory),
		ExtraVars:        strings.TrimSpace(cfg.ExtraVars),
		RequirementsFile: strings.TrimSpace(cfg.RequirementsFile),
		Become:           cfg.Become,
		ReleaseName:      strings.TrimSpace(cfg.ReleaseName),
		HelmNamespace:    strings.TrimSpace(cfg.HelmNamespace),
		Chart:            strings.TrimSpace(cfg.Chart),
		RepoURL:          strings.TrimSpace(cfg.RepoURL),
		ChartVersion:     strings.TrimSpace(cfg.ChartVersion),
		ValuesFiles:      strings.TrimSpace(cfg.ValuesFiles),
		SetValues:        strings.TrimSpace(cfg.SetValues),
		Atomic:           cfg.Atomic,
		Wait:             cfg.Wait,
		CreateNamespace:  cfg.CreateNamespace,
		ToolVersion:      strings.TrimPrefix(strings.TrimSpace(cfg.ToolVersion), "v"),
	}
}

var toolVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func validToolVersion(raw string) error {
	v := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if v == "" {
		return nil
	}
	if !toolVersionPattern.MatchString(v) {
		return fmt.Errorf("%w: tool version must look like 1.2.3", ErrInvalidEngine)
	}
	return nil
}

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validDNSLabel(raw string, max int) error {
	if raw == "" {
		return nil
	}
	if len(raw) > max || !dns1123Label.MatchString(raw) {
		return fmt.Errorf("%w: %q is not a DNS label", ErrInvalidEngine, raw)
	}
	return nil
}

func validRelativePath(raw string) error {
	v := strings.TrimSpace(raw)
	if v == "" || v == "." {
		return nil
	}
	if strings.HasPrefix(v, "-") || strings.Contains(v, "\x00") || filepath.IsAbs(v) {
		return fmt.Errorf("%w: path must be relative to the working directory", ErrInvalidEngine)
	}
	clean := filepath.Clean(v)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: path escapes the workspace", ErrInvalidEngine)
	}
	return nil
}

func splitCSV(raw string) []string {
	raw = strings.ReplaceAll(raw, "\n", ",")
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validChartRef(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: chart is required", ErrInvalidEngine)
	}
	if strings.Contains(raw, "..") || strings.ContainsAny(raw, " \t\n") || strings.HasPrefix(raw, "-") {
		return fmt.Errorf("%w: invalid chart %q", ErrInvalidEngine, raw)
	}
	if strings.HasPrefix(raw, "oci://") {
		return validRepoURL(raw)
	}
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "/") {
		return fmt.Errorf("%w: chart must be a name, relative path, or oci:// reference", ErrInvalidEngine)
	}
	if strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../") {
		return validRelativePath(raw)
	}
	return nil
}

func validRepoURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "oci") {
		return fmt.Errorf("%w: chart repository must be an http(s) or oci URL", ErrInvalidEngine)
	}
	return nil
}

func validChartVersion(raw string) error {
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "-") || strings.ContainsAny(raw, " \t\n") || len(raw) > 64 {
		return fmt.Errorf("%w: invalid chart version", ErrInvalidEngine)
	}
	return nil
}

// ValidateEngineConfig checks the fields used by the selected engine.
func ValidateEngineConfig(engine string, cfg EngineConfig) error {
	if err := validToolVersion(cfg.ToolVersion); err != nil {
		return err
	}
	switch engine {
	case EngineKubernetes:
		if err := validDNSLabel(cfg.K8sNamespace, 63); err != nil {
			return err
		}
		return validRelativePath(cfg.ManifestPath)
	case EngineAnsible:
		if err := validRelativePath(cfg.Playbook); err != nil {
			return err
		}
		if err := validRelativePath(cfg.Inventory); err != nil {
			return err
		}
		return validRelativePath(cfg.RequirementsFile)
	case EngineHelm:
		if err := validDNSLabel(cfg.ReleaseName, 53); err != nil {
			return err
		}
		if cfg.ReleaseName == "" {
			return fmt.Errorf("%w: release name is required", ErrInvalidEngine)
		}
		if err := validChartRef(cfg.Chart); err != nil {
			return err
		}
		if err := validDNSLabel(cfg.HelmNamespace, 63); err != nil {
			return err
		}
		if err := validRepoURL(cfg.RepoURL); err != nil {
			return err
		}
		if err := validChartVersion(cfg.ChartVersion); err != nil {
			return err
		}
		for _, f := range splitCSV(cfg.ValuesFiles) {
			if err := validRelativePath(f); err != nil {
				return err
			}
		}
		for _, s := range splitCSV(cfg.SetValues) {
			if strings.HasPrefix(s, "-") || strings.ContainsAny(s, "\n") {
				return fmt.Errorf("%w: invalid --set value", ErrInvalidEngine)
			}
		}
	}
	return nil
}

func (p *Project) SetEngine(raw string, cfg EngineConfig) error {
	eng, err := ValidateEngine(raw)
	if err != nil {
		return err
	}
	cleaned := CleanEngineConfig(cfg)
	if err := ValidateEngineConfig(eng, cleaned); err != nil {
		return err
	}
	p.s.Engine = eng
	p.s.EngineConfig = cleaned
	return nil
}

func (p *Project) SetPlacement(tenantID string, repoID int64) error {
	if tenantID == "" || repoID <= 0 {
		return fmt.Errorf("placement requires tenant id and positive repository id")
	}
	p.s.TenantID = tenantID
	p.s.GitHubRepoID = repoID
	return nil
}

func (p *Project) SetAutoApprove(v bool) { p.s.AutoApprove = v }

func (p *Project) SetTags(tags map[string]string) error {
	cleaned := make(map[string]string, len(tags))
	for k, v := range tags {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !validTagPart(k) || !validTagPart(v) {
			return fmt.Errorf("%w: %q:%q", ErrInvalidTag, k, v)
		}
		cleaned[k] = v
	}
	p.s.Tags = cleaned
	return nil
}

func ParseTags(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, token := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		k, v, ok := strings.Cut(token, ":")
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrInvalidTag, token)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !validTagPart(k) || !validTagPart(v) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidTag, token)
		}
		out[k] = v
	}
	return out, nil
}

func TagList(tags map[string]string) []string {
	if len(tags) == 0 {
		return nil
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+":"+tags[k])
	}
	return out
}

func FormatTags(tags map[string]string) string {
	return strings.Join(TagList(tags), ", ")
}

func validTagPart(s string) bool {
	if s == "" {
		return false
	}
	return !strings.ContainsAny(s, " \t\n\r:,")
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

func (p *Project) SetStateToken(token string)    { p.s.StateToken = token }
func (p *Project) SetStateTokenHash(hash string) { p.s.StateTokenHash = hash }
func (p *Project) Lock()                         { p.s.Locked = true }
func (p *Project) Unlock()                       { p.s.Locked = false }

func (p *Project) ClaimActiveRun(runID string) error {
	if runID == "" {
		return fmt.Errorf("missing run id")
	}
	if p.s.Locked {
		return ErrLocked
	}
	if p.s.ActiveRunID != "" && p.s.ActiveRunID != runID {
		return ErrBusy
	}
	p.s.ActiveRunID = runID
	return nil
}

func (p *Project) ReleaseActiveRun(runID string) error {
	if p.s.ActiveRunID != runID {
		return ErrActiveRunMismatch
	}
	p.s.ActiveRunID = ""
	return nil
}

func (p *Project) RenameRepo(owner, name string, now time.Time) error {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if owner == "" || name == "" {
		return errors.New("repo owner and name required")
	}
	p.s.Repo.Owner = owner
	p.s.Repo.Name = name
	p.s.UpdatedAt = now
	return nil
}

func (p *Project) SetDriftConfig(enabled bool, intervalMinutes int, now time.Time) {
	if intervalMinutes <= 0 {
		intervalMinutes = DefaultDriftIntervalMinutes
	}
	p.s.DriftEnabled = enabled
	p.s.DriftIntervalMinutes = intervalMinutes
	if p.s.DriftStatus == "" {
		p.s.DriftStatus = DriftStatusUnknown
	}
	if !now.IsZero() {
		p.s.UpdatedAt = now
	}
}

func (p *Project) RecordDriftStatus(status string, checkedAt time.Time) {
	switch status {
	case DriftStatusSynced, DriftStatusDrifted, DriftStatusError, DriftStatusUnknown:
		p.s.DriftStatus = status
	default:
		p.s.DriftStatus = DriftStatusUnknown
	}
	if !checkedAt.IsZero() {
		ts := checkedAt.UTC()
		p.s.LastDriftCheckAt = &ts
		p.s.UpdatedAt = ts
	}
}

func (p *Project) DueForDriftCheck(now time.Time) bool {
	if !p.s.DriftEnabled || p.s.Locked || p.s.ActiveRunID != "" {
		return false
	}
	interval := p.s.DriftIntervalMinutes
	if interval <= 0 {
		interval = DefaultDriftIntervalMinutes
	}
	if p.s.LastDriftCheckAt == nil || p.s.LastDriftCheckAt.IsZero() {
		return true
	}
	return !now.Before(p.s.LastDriftCheckAt.Add(time.Duration(interval) * time.Minute))
}

func (p *Project) SetTTL(ttlHours int, autoDestroy bool, now time.Time) {
	if ttlHours < 0 {
		ttlHours = 0
	}
	prevTTL := p.s.TTLHours
	p.s.TTLHours = ttlHours
	p.s.AutoDestroy = autoDestroy
	if ttlHours == 0 {
		p.s.ExpiresAt = nil
		p.s.AutoDestroy = false
	} else if p.s.ExpiresAt == nil || prevTTL != ttlHours {
		base := now
		if base.IsZero() {
			base = time.Now().UTC()
		}
		exp := base.UTC().Add(time.Duration(ttlHours) * time.Hour)
		p.s.ExpiresAt = &exp
	}
	if !now.IsZero() {
		p.s.UpdatedAt = now
	}
}

func (p *Project) ExtendTTL(extraHours int, now time.Time) {
	if extraHours <= 0 {
		extraHours = 24
	}
	base := now.UTC()
	if p.s.ExpiresAt != nil && p.s.ExpiresAt.After(base) {
		base = p.s.ExpiresAt.UTC()
	}
	exp := base.Add(time.Duration(extraHours) * time.Hour)
	p.s.ExpiresAt = &exp
	if p.s.TTLHours <= 0 {
		p.s.TTLHours = extraHours
	}
	if !now.IsZero() {
		p.s.UpdatedAt = now
	}
}

func (p *Project) IsExpired(now time.Time) bool {
	if !p.s.AutoDestroy || p.s.ExpiresAt == nil || p.s.ExpiresAt.IsZero() {
		return false
	}
	if p.s.Locked || p.s.ActiveRunID != "" {
		return false
	}
	return !now.Before(*p.s.ExpiresAt)
}

func (p *Project) ClearExpiration(now time.Time) {
	p.s.ExpiresAt = nil
	p.s.AutoDestroy = false
	if !now.IsZero() {
		p.s.UpdatedAt = now
	}
}
