package policyapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"infra-guy/internal/domain/policy"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/infrastructure/opa"
	"infra-guy/internal/infrastructure/telemetry"
)

type EvalResult = opa.EvalResult

type Engine interface {
	Validate(code string) error
	Evaluate(ctx context.Context, code string, input map[string]any) (EvalResult, error)
}

type Service struct {
	Policies policy.Repository
	Engine   Engine
	Now      func() time.Time
	NewID    func() string
}

type CreateInput struct {
	TenantID    string
	Name        string
	Description string
	Rego        string
	Tags        map[string]string
}

type UpdateInput struct {
	ID          policy.ID
	TenantID    string
	Name        string
	Description string
	Rego        string
	Tags        map[string]string
}

type UserInfo struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	Role  string `json:"role"`
}

type ApprovalInfo struct {
	ID         string `json:"id"`
	Login      string `json:"login"`
	Role       string `json:"role"`
	ApprovedAt string `json:"approved_at,omitempty"`
}

type ProjectInfo struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	RepoOwner        string            `json:"repo_owner"`
	RepoName         string            `json:"repo_name"`
	WorkingDirectory string            `json:"working_directory"`
	TerraformVersion string            `json:"terraform_version"`
	AutoApprove      bool              `json:"auto_approve"`
	Tags             map[string]string `json:"tags,omitempty"`
}

type ResolvedPolicy struct {
	Policy     *policy.Policy
	Source     string // "explicit", "tag-bound", "global"
	MatchedTag string
}

type PolicyEvalItem struct {
	PolicyID   policy.ID          `json:"policy_id"`
	PolicyName string             `json:"policy_name"`
	Source     string             `json:"source,omitempty"`
	MatchedTag string             `json:"matched_tag,omitempty"`
	Status     opa.DecisionStatus `json:"status"`
	Allowed    bool               `json:"allowed"`
	Undecided  bool               `json:"undecided"`
	Deny       []string           `json:"deny"`
}

type ApprovalDecision struct {
	PolicyID   policy.ID          `json:"policy_id"`
	PolicyName string             `json:"policy_name"`
	Source     string             `json:"source,omitempty"`
	MatchedTag string             `json:"matched_tag,omitempty"`
	Status     opa.DecisionStatus `json:"status"`
	Allowed    bool               `json:"allowed"`
	Undecided  bool               `json:"undecided"`
	Deny       []string           `json:"deny"`
	Policies   []PolicyEvalItem   `json:"policies,omitempty"`
}

type RunPolicyDecision = ApprovalDecision

func (s *Service) engine() Engine {
	if s.Engine != nil {
		return s.Engine
	}
	return opa.NewEvaluator()
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	return fmt.Sprintf("pol-%d", s.now().UnixNano())
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*policy.Policy, error) {
	p, err := policy.New(policy.NewInput{
		ID:          policy.ID(s.newID()),
		TenantID:    in.TenantID,
		Name:        in.Name,
		Description: in.Description,
		Rego:        in.Rego,
		Tags:        in.Tags,
		Now:         s.now(),
	})
	if err != nil {
		return nil, err
	}
	if err := s.engine().Validate(in.Rego); err != nil {
		return nil, fmt.Errorf("%w: %v", policy.ErrInvalidRego, err)
	}
	if err := s.Policies.Insert(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Service) Update(ctx context.Context, in UpdateInput) (*policy.Policy, error) {
	p, err := s.Get(ctx, in.TenantID, in.ID)
	if err != nil {
		return nil, err
	}
	if err := p.Update(in.Name, in.Description, in.Rego, s.now()); err != nil {
		return nil, err
	}
	if in.Tags != nil {
		p.SetTags(in.Tags)
	}
	if err := s.engine().Validate(in.Rego); err != nil {
		return nil, fmt.Errorf("%w: %v", policy.ErrInvalidRego, err)
	}
	if err := s.Policies.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Service) Delete(ctx context.Context, tenantID string, id policy.ID) error {
	if _, err := s.Get(ctx, tenantID, id); err != nil {
		return err
	}
	return s.Policies.Delete(ctx, id)
}

func (s *Service) Get(ctx context.Context, tenantID string, id policy.ID) (*policy.Policy, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, policy.ErrNotFound
	}
	p, err := s.Policies.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil || p.TenantID() != tenantID {
		return nil, policy.ErrNotFound
	}
	return p, nil
}

func (s *Service) ListByTenant(ctx context.Context, tenantID string) ([]*policy.Policy, error) {
	return s.Policies.ListByTenant(ctx, tenantID)
}

func (s *Service) List(ctx context.Context, tenantID string) ([]*policy.Policy, error) {
	return s.ListByTenant(ctx, tenantID)
}

func (s *Service) TestRego(ctx context.Context, regoCode string, rawJSON []byte) (EvalResult, error) {
	if strings.TrimSpace(regoCode) == "" {
		return EvalResult{}, policy.ErrInvalidRego
	}
	var inputMap map[string]any
	trimmed := bytes.TrimSpace(rawJSON)
	if len(trimmed) == 0 {
		inputMap = map[string]any{}
	} else {
		if err := json.Unmarshal(trimmed, &inputMap); err != nil {
			return EvalResult{}, fmt.Errorf("invalid input json: %w", err)
		}
		if inputMap == nil {
			inputMap = map[string]any{}
		}
	}
	ensurePlanAliases(inputMap)
	return s.engine().Evaluate(ctx, regoCode, inputMap)
}

func (s *Service) BuildInput(planJSON []byte, u UserInfo, p ProjectInfo, approvals ...[]ApprovalInfo) (map[string]any, string, error) {
	var planObj any = map[string]any{}
	trimmed := bytes.TrimSpace(planJSON)
	if len(trimmed) > 0 {
		if err := json.Unmarshal(trimmed, &planObj); err != nil {
			return nil, "", fmt.Errorf("invalid plan json: %w", err)
		}
		if planObj == nil {
			planObj = map[string]any{}
		}
	}

	userMap := map[string]any{
		"id":    u.ID,
		"login": u.Login,
		"role":  u.Role,
	}
	projectMap := map[string]any{
		"id":                p.ID,
		"name":              p.Name,
		"repo_owner":        p.RepoOwner,
		"repo_name":         p.RepoName,
		"working_directory": p.WorkingDirectory,
		"terraform_version": p.TerraformVersion,
		"auto_approve":      p.AutoApprove,
	}
	if len(p.Tags) > 0 {
		tagsMap := make(map[string]any, len(p.Tags))
		for k, v := range p.Tags {
			tagsMap[k] = v
		}
		projectMap["tags"] = tagsMap
	}
	var flatApprovals []ApprovalInfo
	for _, list := range approvals {
		flatApprovals = append(flatApprovals, list...)
	}
	approvalMaps := make([]map[string]any, 0, len(flatApprovals))
	for _, a := range flatApprovals {
		entry := map[string]any{
			"id":    a.ID,
			"login": a.Login,
			"role":  a.Role,
		}
		if a.ApprovedAt != "" {
			entry["approved_at"] = a.ApprovedAt
		}
		approvalMaps = append(approvalMaps, entry)
	}

	costEst := run.EstimatePlanCost(trimmed)
	if costEst == nil {
		costEst = &run.CostEstimate{Currency: "USD"}
	}
	costResources := make([]map[string]any, 0, len(costEst.Resources))
	for _, rc := range costEst.Resources {
		costResources = append(costResources, map[string]any{
			"address":           rc.Address,
			"resource_type":     rc.ResourceType,
			"action":            rc.Action,
			"sku":               rc.SKU,
			"prev_monthly_cost": rc.PrevMonthlyCost,
			"monthly_cost":      rc.MonthlyCost,
			"diff_monthly_cost": rc.DiffMonthlyCost,
		})
	}
	costMap := map[string]any{
		"prev_monthly_cost": costEst.PrevMonthlyCost,
		"monthly_cost":      costEst.MonthlyCost,
		"diff_monthly_cost": costEst.DiffMonthlyCost,
		"currency":          costEst.Currency,
		"resources":         costResources,
	}

	k8sObjects := kubernetesObjectsFromPlan(planObj)
	k8sMap := map[string]any{"objects": k8sObjects}

	displayPayload := map[string]any{
		"plan":         planObj,
		"kubernetes":   k8sMap,
		"cost":         costMap,
		"approvals":    approvalMaps,
		"current_user": userMap,
		"project":      projectMap,
	}
	prettyBytes, err := json.MarshalIndent(displayPayload, "", "  ")
	if err != nil {
		return nil, "", err
	}

	evalInput := map[string]any{
		"plan":         planObj,
		"kubernetes":   k8sMap,
		"cost":         costMap,
		"approvals":    approvalMaps,
		"reviews":      map[string]any{"approvals": approvalMaps},
		"current_user": userMap,
		"project":      projectMap,
		"input": map[string]any{
			"plan":       planObj,
			"kubernetes": k8sMap,
			"cost":       costMap,
			"approvals":  approvalMaps,
		},
	}
	return evalInput, string(prettyBytes), nil
}

func (s *Service) EvaluateApproval(ctx context.Context, tenantID string, policyID policy.ID, planJSON []byte, u UserInfo, p ProjectInfo, approvals ...[]ApprovalInfo) (*ApprovalDecision, error) {
	if strings.TrimSpace(string(policyID)) == "" {
		return &ApprovalDecision{
			Status:  opa.StatusAllowed,
			Allowed: true,
			Deny:    []string{},
		}, nil
	}
	pol, err := s.Get(ctx, tenantID, policyID)
	if err != nil {
		return nil, err
	}
	inputMap, _, err := s.BuildInput(planJSON, u, p, approvals...)
	if err != nil {
		return nil, err
	}
	evalCtx, span := telemetry.Default().StartSpan(ctx, "opa.evaluate",
		telemetry.Attr("tenant.id", tenantID),
		telemetry.Attr("policy.id", string(pol.ID())),
		telemetry.Attr("policy.name", pol.Name()),
	)
	start := time.Now()
	res, err := s.engine().Evaluate(evalCtx, pol.Rego(), inputMap)
	dur := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.End()
		telemetry.Default().RecordPolicyEvaluation(tenantID, p.ID, p.Name, "mandatory", "error", dur)
		return nil, err
	}
	span.SetAttribute("policy.decision", string(res.Status))
	span.End()
	telemetry.Default().RecordPolicyEvaluation(tenantID, p.ID, p.Name, "mandatory", string(res.Status), dur)
	return &ApprovalDecision{
		PolicyID:   pol.ID(),
		PolicyName: pol.Name(),
		Status:     res.Status,
		Allowed:    res.Allowed,
		Undecided:  res.Undecided,
		Deny:       res.Deny,
	}, nil
}

func (s *Service) EvaluateForRun(ctx context.Context, tenantID string, policyID policy.ID, planJSON []byte, u UserInfo, p ProjectInfo, approvals ...[]ApprovalInfo) (*RunPolicyDecision, error) {
	return s.EvaluateApproval(ctx, tenantID, policyID, planJSON, u, p, approvals...)
}

// ResolveForProject returns all policies that apply to a workspace:
//  1. The explicit policy ids, in order, skipping blanks and duplicates
//  2. Any tenant policies whose tags match the workspace tags (tag-bound or global scope),
//     deduplicated by policy ID and ordered deterministically.
func (s *Service) ResolveForProject(ctx context.Context, tenantID string, explicitIDs []policy.ID, projectTags map[string]string) ([]ResolvedPolicy, error) {
	seen := make(map[policy.ID]bool)
	var out []ResolvedPolicy

	for _, rawID := range explicitIDs {
		explicitID := policy.ID(strings.TrimSpace(string(rawID)))
		if explicitID == "" || seen[explicitID] {
			continue
		}
		pol, err := s.Get(ctx, tenantID, explicitID)
		if err != nil {
			return nil, err
		}
		seen[explicitID] = true
		seen[pol.ID()] = true
		out = append(out, ResolvedPolicy{
			Policy: pol,
			Source: "explicit",
		})
	}

	if strings.TrimSpace(tenantID) == "" || s.Policies == nil {
		return out, nil
	}
	all, err := s.Policies.ListByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CreatedAt().Equal(all[j].CreatedAt()) {
			return string(all[i].ID()) < string(all[j].ID())
		}
		return all[i].CreatedAt().Before(all[j].CreatedAt())
	})
	for _, pol := range all {
		if pol == nil || seen[pol.ID()] {
			continue
		}
		matched, source, matchedTag := policy.MatchesProjectTags(pol.Tags(), projectTags)
		if !matched {
			continue
		}
		seen[pol.ID()] = true
		out = append(out, ResolvedPolicy{
			Policy:     pol,
			Source:     source,
			MatchedTag: matchedTag,
		})
	}
	return out, nil
}

// EvaluateForProject evaluates all policies applicable to a workspace (explicit + tag-bound + global).
// Every applicable policy must allow the run for the aggregate decision to be Allowed.
// Any deny across any policy results in StatusDenied with all deny messages aggregated.
func (s *Service) EvaluateForProject(ctx context.Context, tenantID string, explicitIDs []policy.ID, projectTags map[string]string, planJSON []byte, u UserInfo, p ProjectInfo, approvals ...[]ApprovalInfo) (*ApprovalDecision, error) {
	resolved, err := s.ResolveForProject(ctx, tenantID, explicitIDs, projectTags)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return &ApprovalDecision{
			Status:  opa.StatusAllowed,
			Allowed: true,
			Deny:    []string{},
		}, nil
	}
	if len(p.Tags) == 0 && len(projectTags) > 0 {
		p.Tags = projectTags
	}
	inputMap, _, err := s.BuildInput(planJSON, u, p, approvals...)
	if err != nil {
		return nil, err
	}

	items := make([]PolicyEvalItem, 0, len(resolved))
	var allDeny []string
	anyDenied := false
	anyUndecided := false

	for _, rp := range resolved {
		evalCtx, span := telemetry.Default().StartSpan(ctx, "opa.evaluate",
			telemetry.Attr("tenant.id", tenantID),
			telemetry.Attr("policy.id", string(rp.Policy.ID())),
			telemetry.Attr("policy.name", rp.Policy.Name()),
			telemetry.Attr("policy.source", rp.Source),
		)
		start := time.Now()
		res, evalErr := s.engine().Evaluate(evalCtx, rp.Policy.Rego(), inputMap)
		dur := time.Since(start)
		if evalErr != nil {
			span.RecordError(evalErr)
			span.End()
			telemetry.Default().RecordPolicyEvaluation(tenantID, p.ID, p.Name, "mandatory", "error", dur)
			return nil, fmt.Errorf("policy %s (%s): %w", rp.Policy.Name(), rp.Policy.ID(), evalErr)
		}
		span.SetAttribute("policy.decision", string(res.Status))
		span.End()
		telemetry.Default().RecordPolicyEvaluation(tenantID, p.ID, p.Name, "mandatory", string(res.Status), dur)
		item := PolicyEvalItem{
			PolicyID:   rp.Policy.ID(),
			PolicyName: rp.Policy.Name(),
			Source:     rp.Source,
			MatchedTag: rp.MatchedTag,
			Status:     res.Status,
			Allowed:    res.Allowed,
			Undecided:  res.Undecided,
			Deny:       res.Deny,
		}
		items = append(items, item)
		if !res.Allowed {
			if res.Undecided {
				anyUndecided = true
			} else {
				anyDenied = true
			}
		}
		for _, d := range res.Deny {
			if len(resolved) > 1 {
				allDeny = append(allDeny, fmt.Sprintf("[%s] %s", rp.Policy.Name(), d))
			} else {
				allDeny = append(allDeny, d)
			}
		}
	}

	primary := items[0]
	for _, it := range items {
		if !it.Allowed && !it.Undecided {
			primary = it
			break
		}
	}
	if primary.Allowed {
		for _, it := range items {
			if it.Undecided {
				primary = it
				break
			}
		}
	}

	aggStatus := opa.StatusAllowed
	aggAllowed := true
	aggUndecided := false
	if anyDenied {
		aggStatus = opa.StatusDenied
		aggAllowed = false
		aggUndecided = false
	} else if anyUndecided {
		aggStatus = opa.StatusUndecided
		aggAllowed = false
		aggUndecided = true
	}
	if allDeny == nil {
		allDeny = []string{}
	}

	return &ApprovalDecision{
		PolicyID:   primary.PolicyID,
		PolicyName: primary.PolicyName,
		Source:     primary.Source,
		MatchedTag: primary.MatchedTag,
		Status:     aggStatus,
		Allowed:    aggAllowed,
		Undecided:  aggUndecided,
		Deny:       allDeny,
		Policies:   items,
	}, nil
}

func ensurePlanAliases(inputMap map[string]any) {
	planVal, hasPlan := inputMap["plan"]
	if nested, ok := inputMap["input"].(map[string]any); ok {
		if !hasPlan {
			if nestedPlan, exists := nested["plan"]; exists {
				inputMap["plan"] = nestedPlan
			}
		} else if _, exists := nested["plan"]; !exists {
			nested["plan"] = planVal
		}
		return
	}
	if hasPlan {
		inputMap["input"] = map[string]any{"plan": planVal}
		return
	}
	emptyPlan := map[string]any{}
	inputMap["plan"] = emptyPlan
	inputMap["input"] = map[string]any{"plan": emptyPlan}
}
