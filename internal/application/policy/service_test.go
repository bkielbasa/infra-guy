package policyapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"infra-guy/internal/domain/policy"
	"infra-guy/internal/infrastructure/opa"
)

type memPolicyRepo struct {
	byID map[policy.ID]*policy.Policy
}

func newMemPolicyRepo() *memPolicyRepo {
	return &memPolicyRepo{byID: make(map[policy.ID]*policy.Policy)}
}

func (r *memPolicyRepo) Insert(_ context.Context, p *policy.Policy) error {
	for _, existing := range r.byID {
		if existing.TenantID() == p.TenantID() && existing.Name() == p.Name() {
			return policy.ErrDuplicateName
		}
	}
	cp := policy.Rehydrate(p.Snapshot())
	r.byID[p.ID()] = cp
	return nil
}

func (r *memPolicyRepo) Update(_ context.Context, p *policy.Policy) error {
	if _, ok := r.byID[p.ID()]; !ok {
		return policy.ErrNotFound
	}
	for _, existing := range r.byID {
		if existing.ID() != p.ID() && existing.TenantID() == p.TenantID() && existing.Name() == p.Name() {
			return policy.ErrDuplicateName
		}
	}
	r.byID[p.ID()] = policy.Rehydrate(p.Snapshot())
	return nil
}

func (r *memPolicyRepo) Delete(_ context.Context, id policy.ID) error {
	if _, ok := r.byID[id]; !ok {
		return policy.ErrNotFound
	}
	delete(r.byID, id)
	return nil
}

func (r *memPolicyRepo) Get(_ context.Context, id policy.ID) (*policy.Policy, error) {
	p, ok := r.byID[id]
	if !ok {
		return nil, nil
	}
	return policy.Rehydrate(p.Snapshot()), nil
}

func (r *memPolicyRepo) ListByTenant(_ context.Context, tenantID string) ([]*policy.Policy, error) {
	var out []*policy.Policy
	for _, p := range r.byID {
		if p.TenantID() == tenantID {
			out = append(out, policy.Rehydrate(p.Snapshot()))
		}
	}
	return out, nil
}

func TestServiceCRUDAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	repo := newMemPolicyRepo()
	seq := 0
	now := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)

	svc := &Service{
		Policies: repo,
		Engine:   opa.NewEvaluator(),
		Now:      func() time.Time { return now },
		NewID: func() string {
			seq++
			return fmt.Sprintf("pol-%d", seq)
		},
	}

	validRego := `
package infraguy

default allow = false

allow if {
	input.current_user.login == "alice"
}
`

	if _, err := svc.Create(ctx, CreateInput{
		TenantID: "t1",
		Name:     "Bad Policy",
		Rego:     "package infraguy\nallow if { ==",
	}); !errors.Is(err, policy.ErrInvalidRego) {
		t.Fatalf("expected ErrInvalidRego on syntax error, got %v", err)
	}

	pol, err := svc.Create(ctx, CreateInput{
		TenantID:    "t1",
		Name:        "  Alice Approver  ",
		Description: "  Only alice can approve  ",
		Rego:        validRego,
	})
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	if pol.Name() != "Alice Approver" || pol.Description() != "Only alice can approve" {
		t.Fatalf("unexpected policy fields: %+v", pol.Snapshot())
	}

	if _, err := svc.Get(ctx, "t2", pol.ID()); !errors.Is(err, policy.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for wrong tenant, got %v", err)
	}

	updatedRego := `
package infraguy

default allow = false

allow if {
	input.current_user.role == "admin"
}
`
	updated, err := svc.Update(ctx, UpdateInput{
		ID:          pol.ID(),
		TenantID:    "t1",
		Name:        "Admin Approver",
		Description: "Admins only",
		Rego:        updatedRego,
	})
	if err != nil {
		t.Fatalf("update policy: %v", err)
	}
	if updated.Name() != "Admin Approver" {
		t.Fatalf("expected updated name, got %s", updated.Name())
	}

	if _, err := svc.Update(ctx, UpdateInput{
		ID:       pol.ID(),
		TenantID: "t2",
		Name:     "Hijack",
		Rego:     updatedRego,
	}); !errors.Is(err, policy.ErrNotFound) {
		t.Fatalf("expected ErrNotFound when updating from wrong tenant, got %v", err)
	}

	list, err := svc.ListByTenant(ctx, "t1")
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 policy for t1, got %d (err=%v)", len(list), err)
	}

	if err := svc.Delete(ctx, "t2", pol.ID()); !errors.Is(err, policy.ErrNotFound) {
		t.Fatalf("expected ErrNotFound when deleting from wrong tenant, got %v", err)
	}
	if err := svc.Delete(ctx, "t1", pol.ID()); err != nil {
		t.Fatalf("delete policy: %v", err)
	}
	if _, err := svc.Get(ctx, "t1", pol.ID()); !errors.Is(err, policy.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestServiceTestRegoAndEvaluateApproval(t *testing.T) {
	ctx := context.Background()
	repo := newMemPolicyRepo()
	svc := &Service{
		Policies: repo,
		Engine:   opa.NewEvaluator(),
		Now:      func() time.Time { return time.Now().UTC() },
		NewID:    func() string { return "pol-approval-1" },
	}

	regoCode := `
package infraguy

default allow = false

allow if {
	rc := input.input.plan.resource_changes[_]
	rc.type == "google_service_account"
	rc.change.actions[_] == "create"
	input.current_user.login == "john"
}

deny contains msg if {
	rc := input.plan.resource_changes[_]
	rc.change.actions[_] == "delete"
	msg := sprintf("cannot delete %s", [rc.address])
}
`

	pol, err := svc.Create(ctx, CreateInput{
		TenantID: "t1",
		Name:     "Service Account Policy",
		Rego:     regoCode,
	})
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}

	planCreate := []byte(`{
		"format_version": "1.2",
		"resource_changes": [
			{
				"address": "google_service_account.sa",
				"type": "google_service_account",
				"change": {"actions": ["create"]}
			}
		]
	}`)

	john := UserInfo{ID: "u1", Login: "john", Role: "member"}
	proj := ProjectInfo{
		ID:               "proj-1",
		Name:             "payments-infra",
		RepoOwner:        "acme",
		RepoName:         "infra",
		WorkingDirectory: "envs/prod",
		TerraformVersion: "1.9.8",
		AutoApprove:      false,
	}

	_, prettyJSON, err := svc.BuildInput(planCreate, john, proj)
	if err != nil {
		t.Fatalf("BuildInput: %v", err)
	}

	var parsedPretty map[string]any
	if err := json.Unmarshal([]byte(prettyJSON), &parsedPretty); err != nil {
		t.Fatalf("pretty JSON should be valid JSON: %v", err)
	}

	// TestRego with the pretty JSON copied from UI (top-level "plan")
	testRes, err := svc.TestRego(ctx, regoCode, []byte(prettyJSON))
	if err != nil {
		t.Fatalf("TestRego with prettyJSON: %v", err)
	}
	if !testRes.Allowed || len(testRes.Deny) != 0 {
		t.Fatalf("expected Allowed=true from TestRego, got %+v", testRes)
	}

	// TestRego with nested {"input": {"plan": ...}}
	nestedJSON := []byte(`{
		"input": {
			"plan": {
				"resource_changes": [
					{
						"address": "google_service_account.sa",
						"type": "google_service_account",
						"change": {"actions": ["create"]}
					}
				]
			}
		},
		"current_user": {"login": "john"}
	}`)
	nestedRes, err := svc.TestRego(ctx, regoCode, nestedJSON)
	if err != nil {
		t.Fatalf("TestRego with nestedJSON: %v", err)
	}
	if !nestedRes.Allowed {
		t.Fatalf("expected Allowed=true for nestedJSON, got %+v", nestedRes)
	}

	// EvaluateApproval for john -> Allowed
	decJohn, err := svc.EvaluateApproval(ctx, "t1", pol.ID(), planCreate, john, proj)
	if err != nil {
		t.Fatalf("EvaluateApproval john: %v", err)
	}
	if !decJohn.Allowed || decJohn.PolicyName != "Service Account Policy" {
		t.Fatalf("expected john allowed, got %+v", decJohn)
	}

	// EvaluateApproval with a delete change -> Denied with reason
	planDelete := []byte(`{
		"resource_changes": [
			{
				"address": "google_service_account.sa",
				"type": "google_service_account",
				"change": {"actions": ["create", "delete"]}
			}
		]
	}`)
	decDelete, err := svc.EvaluateApproval(ctx, "t1", pol.ID(), planDelete, john, proj)
	if err != nil {
		t.Fatalf("EvaluateApproval delete: %v", err)
	}
	if decDelete.Allowed || len(decDelete.Deny) != 1 || decDelete.Deny[0] != "cannot delete google_service_account.sa" {
		t.Fatalf("expected denied with message, got %+v", decDelete)
	}
}

func TestTagBoundAndMultiPolicyEvaluation(t *testing.T) {
	ctx := context.Background()
	repo := newMemPolicyRepo()
	seq := 0
	svc := &Service{
		Policies: repo,
		Engine:   opa.NewEvaluator(),
		Now:      func() time.Time { return time.Now().UTC() },
		NewID: func() string {
			seq++
			return fmt.Sprintf("pol-%d", seq)
		},
	}

	// 1. Global policy: no iam deletes
	globalPol, err := svc.Create(ctx, CreateInput{
		TenantID: "t1",
		Name:     "Global No IAM Delete",
		Tags:     map[string]string{"scope": "global"},
		Rego: `package infraguy.approval
import rego.v1
default allow := true
deny contains "global: cannot delete iam resource" if {
	some rc in input.plan.resource_changes
	rc.type == "aws_iam_role"
	"delete" in rc.change.actions
}
`,
	})
	if err != nil {
		t.Fatalf("create global policy: %v", err)
	}

	// 2. Tag-bound policy (env:prod): requires admin role
	prodPol, err := svc.Create(ctx, CreateInput{
		TenantID: "t1",
		Name:     "Prod Requires Admin",
		Tags:     map[string]string{"env": "prod"},
		Rego: `package infraguy.approval
import rego.v1
default allow := false
allow if {
	input.current_user.role == "admin"
}
deny contains "prod: only admins can approve" if {
	input.current_user.role != "admin"
}
`,
	})
	if err != nil {
		t.Fatalf("create prod policy: %v", err)
	}

	// 3. Explicit workspace policy: requires project tag team=platform
	explicitPol, err := svc.Create(ctx, CreateInput{
		TenantID: "t1",
		Name:     "Platform Team Policy",
		Rego: `package infraguy.approval
import rego.v1
default allow := false
allow if {
	input.project.tags.team == "platform"
}
deny contains "explicit: project must have team:platform tag" if {
	input.project.tags.team != "platform"
}
`,
	})
	if err != nil {
		t.Fatalf("create explicit policy: %v", err)
	}

	// Resolve policies for a project with tags env:prod, team:platform and explicitPol attached
	projectTags := map[string]string{"env": "prod", "team": "platform"}
	resolved, err := svc.ResolveForProject(ctx, "t1", explicitPol.ID(), projectTags)
	if err != nil {
		t.Fatalf("ResolveForProject: %v", err)
	}
	if len(resolved) != 3 {
		t.Fatalf("expected 3 resolved policies (explicit + tag + global), got %d: %+v", len(resolved), resolved)
	}
	if resolved[0].Policy.ID() != explicitPol.ID() || resolved[0].Source != "explicit" {
		t.Fatalf("expected first policy to be explicit, got %+v", resolved[0])
	}

	safePlan := []byte(`{"resource_changes":[{"address":"aws_s3_bucket.b","type":"aws_s3_bucket","change":{"actions":["create"]}}]}`)
	proj := ProjectInfo{
		ID:   "p-prod",
		Name: "prod-core",
		Tags: projectTags,
	}

	// Evaluate as non-admin member -> denied by Prod Requires Admin (tag-bound), even though explicit & global pass!
	memberUser := UserInfo{ID: "u-mem", Login: "bob", Role: "member"}
	decMember, err := svc.EvaluateForProject(ctx, "t1", explicitPol.ID(), projectTags, safePlan, memberUser, proj)
	if err != nil {
		t.Fatalf("EvaluateForProject member: %v", err)
	}
	if decMember.Allowed {
		t.Fatalf("expected member to be denied by tag-bound prod policy, got %+v", decMember)
	}
	if len(decMember.Policies) != 3 {
		t.Fatalf("expected 3 policy eval items, got %d", len(decMember.Policies))
	}

	// Evaluate as admin -> all 3 policies allow!
	adminUser := UserInfo{ID: "u-adm", Login: "alice", Role: "admin"}
	decAdmin, err := svc.EvaluateForProject(ctx, "t1", explicitPol.ID(), projectTags, safePlan, adminUser, proj)
	if err != nil {
		t.Fatalf("EvaluateForProject admin: %v", err)
	}
	if !decAdmin.Allowed || len(decAdmin.Deny) != 0 {
		t.Fatalf("expected admin to be allowed across all 3 policies, got %+v", decAdmin)
	}

	// Evaluate as admin with IAM delete -> denied by Global policy!
	iamDeletePlan := []byte(`{"resource_changes":[{"address":"aws_iam_role.r","type":"aws_iam_role","change":{"actions":["delete"]}}]}`)
	decIAM, err := svc.EvaluateForProject(ctx, "t1", explicitPol.ID(), projectTags, iamDeletePlan, adminUser, proj)
	if err != nil {
		t.Fatalf("EvaluateForProject iam delete: %v", err)
	}
	if decIAM.Allowed || len(decIAM.Deny) != 1 {
		t.Fatalf("expected global policy to deny IAM delete, got %+v", decIAM)
	}
	_ = globalPol
	_ = prodPol
}

