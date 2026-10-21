package opa

import (
	"context"
	"reflect"
	"testing"
)

func TestEvaluatorValidate(t *testing.T) {
	ev := NewEvaluator()

	t.Run("valid rego v1", func(t *testing.T) {
		code := `
package infraguy

default allow = false

allow if {
	input.current_user.login == "alice"
}
`
		if err := ev.Validate(code); err != nil {
			t.Fatalf("expected valid rego v1, got error: %v", err)
		}
	})

	t.Run("valid rego v0", func(t *testing.T) {
		code := `
package infraguy

default allow = false

allow {
	input.current_user.login == "alice"
}

deny[msg] {
	input.current_user.login == "mallory"
	msg := "mallory is blocked"
}
`
		if err := ev.Validate(code); err != nil {
			t.Fatalf("expected valid rego v0, got error: %v", err)
		}
	})

	t.Run("empty code rejected", func(t *testing.T) {
		if err := ev.Validate("   "); err == nil {
			t.Fatalf("expected error for empty policy")
		}
	})

	t.Run("syntax error rejected", func(t *testing.T) {
		if err := ev.Validate("package infraguy\nallow if { == }"); err == nil {
			t.Fatalf("expected syntax error")
		}
	})
}

func TestEvaluatorEvaluate(t *testing.T) {
	ev := NewEvaluator()
	ctx := context.Background()

	t.Run("allow rule with input.current_user.login and input.plan", func(t *testing.T) {
		code := `
package infraguy

default allow = false

allow if {
	input.current_user.login == "alice"
	count(input.plan.resource_changes) == 1
}
`
		plan := map[string]any{
			"resource_changes": []any{
				map[string]any{
					"address": "null_resource.example",
					"change":  map[string]any{"actions": []any{"create"}},
				},
			},
		}

		resAlice, err := ev.Evaluate(ctx, code, map[string]any{
			"plan":         plan,
			"current_user": map[string]any{"login": "alice"},
		})
		if err != nil {
			t.Fatalf("evaluate alice: %v", err)
		}
		if !resAlice.Allowed || len(resAlice.Deny) != 0 {
			t.Fatalf("expected alice allowed with no deny, got %+v", resAlice)
		}

		resBob, err := ev.Evaluate(ctx, code, map[string]any{
			"plan":         plan,
			"current_user": map[string]any{"login": "bob"},
		})
		if err != nil {
			t.Fatalf("evaluate bob: %v", err)
		}
		if resBob.Allowed {
			t.Fatalf("expected bob denied, got %+v", resBob)
		}
	})

	t.Run("issue 2 nested input.input.plan.resource_changes compatibility", func(t *testing.T) {
		code := `
package infraguy

default allow = false

allow if {
	rc := input.input.plan.resource_changes[_]
	rc.type == "google_Service_account"
	rc.change.actions[_] == "create"
	input.current_user.login == "john"
}
`
		rawInput := map[string]any{
			"plan": map[string]any{
				"resource_changes": []any{
					map[string]any{
						"type":   "google_Service_account",
						"change": map[string]any{"actions": []any{"create"}},
					},
				},
			},
			"current_user": map[string]any{"login": "john"},
		}
		res, err := ev.Evaluate(ctx, code, rawInput)
		if err != nil {
			t.Fatalf("evaluate issue #2 snippet: %v", err)
		}
		if !res.Allowed {
			t.Fatalf("expected Allowed=true for john, got %+v", res)
		}
	})

	t.Run("nested input.plan copied to top-level plan when top-level plan missing", func(t *testing.T) {
		code := `
package custom.approval

allow if {
	input.plan.format_version == "1.2"
}
`
		rawInput := map[string]any{
			"input": map[string]any{
				"plan": map[string]any{"format_version": "1.2"},
			},
		}
		res, err := ev.Evaluate(ctx, code, rawInput)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if !res.Allowed {
			t.Fatalf("expected Allowed=true, got %+v", res)
		}
	})

	t.Run("deny rules override allow and return sorted messages", func(t *testing.T) {
		code := `
package infraguy

default allow = true

deny contains msg if {
	input.project.name == "prod"
	msg := "zeta: prod changes require release window"
}

deny contains msg if {
	input.current_user.role != "admin"
	msg := "alpha: only admins can approve"
}
`
		res, err := ev.Evaluate(ctx, code, map[string]any{
			"project":      map[string]any{"name": "prod"},
			"current_user": map[string]any{"login": "dev", "role": "member"},
		})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if res.Allowed {
			t.Fatalf("expected Allowed=false when deny messages present")
		}
		want := []string{
			"alpha: only admins can approve",
			"zeta: prod changes require release window",
		}
		if !reflect.DeepEqual(res.Deny, want) {
			t.Fatalf("expected deny=%v, got %v", want, res.Deny)
		}
	})

	t.Run("policy with only deny rules and no allow rule", func(t *testing.T) {
		code := `
package infraguy

deny contains "cannot destroy resources" if {
	rc := input.plan.resource_changes[_]
	rc.change.actions[_] == "delete"
}
`
		resSafe, err := ev.Evaluate(ctx, code, map[string]any{
			"plan": map[string]any{
				"resource_changes": []any{
					map[string]any{"change": map[string]any{"actions": []any{"create"}}},
				},
			},
		})
		if err != nil {
			t.Fatalf("evaluate safe plan: %v", err)
		}
		if !resSafe.Allowed || len(resSafe.Deny) != 0 {
			t.Fatalf("expected safe plan Allowed=true, got %+v", resSafe)
		}

		resDestroy, err := ev.Evaluate(ctx, code, map[string]any{
			"plan": map[string]any{
				"resource_changes": []any{
					map[string]any{"change": map[string]any{"actions": []any{"delete"}}},
				},
			},
		})
		if err != nil {
			t.Fatalf("evaluate destroy plan: %v", err)
		}
		if resDestroy.Allowed || len(resDestroy.Deny) != 1 || resDestroy.Deny[0] != "cannot destroy resources" {
			t.Fatalf("expected destroy plan denied, got %+v", resDestroy)
		}
	})
}
