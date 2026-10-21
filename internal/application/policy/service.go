package policyapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"infra-guy/internal/domain/policy"
	"infra-guy/internal/infrastructure/opa"
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
}

type UpdateInput struct {
	ID          policy.ID
	TenantID    string
	Name        string
	Description string
	Rego        string
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
	ID               string `json:"id"`
	Name             string `json:"name"`
	RepoOwner        string `json:"repo_owner"`
	RepoName         string `json:"repo_name"`
	WorkingDirectory string `json:"working_directory"`
	TerraformVersion string `json:"terraform_version"`
	AutoApprove      bool   `json:"auto_approve"`
}

type ApprovalDecision struct {
	PolicyID   policy.ID          `json:"policy_id"`
	PolicyName string             `json:"policy_name"`
	Status     opa.DecisionStatus `json:"status"`
	Allowed    bool               `json:"allowed"`
	Undecided  bool               `json:"undecided"`
	Deny       []string           `json:"deny"`
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

	displayPayload := map[string]any{
		"plan":         planObj,
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
		"approvals":    approvalMaps,
		"reviews":      map[string]any{"approvals": approvalMaps},
		"current_user": userMap,
		"project":      projectMap,
		"input": map[string]any{
			"plan":      planObj,
			"approvals": approvalMaps,
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
	res, err := s.engine().Evaluate(ctx, pol.Rego(), inputMap)
	if err != nil {
		return nil, err
	}
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
