package opa

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
)

var ErrEmptyPolicy = errors.New("rego policy cannot be empty")

type EvalResult struct {
	Allowed bool     `json:"allowed"`
	Deny    []string `json:"deny"`
}

type Evaluator struct{}

func NewEvaluator() *Evaluator {
	return &Evaluator{}
}

// ParseAndValidate parses a Rego module trying Rego v1 first and falling back to Rego v0
// so both modern (`allow if { ... }`, `deny contains msg if { ... }`) and legacy (`allow { ... }`, `deny[msg] { ... }`)
// policy snippets are supported.
func ParseAndValidate(code string) (*ast.Module, ast.RegoVersion, error) {
	if strings.TrimSpace(code) == "" {
		return nil, ast.RegoUndefined, ErrEmptyPolicy
	}
	mod, errV1 := ast.ParseModuleWithOpts("policy.rego", code, ast.ParserOptions{
		RegoVersion: ast.RegoV1,
	})
	if errV1 == nil {
		return mod, ast.RegoV1, nil
	}
	modV0, errV0 := ast.ParseModuleWithOpts("policy.rego", code, ast.ParserOptions{
		RegoVersion: ast.RegoV0,
	})
	if errV0 == nil {
		return modV0, ast.RegoV0, nil
	}
	return nil, ast.RegoUndefined, errV1
}

func ValidateRego(code string) error {
	_, _, err := ParseAndValidate(code)
	return err
}

func (e *Evaluator) Validate(code string) error {
	return ValidateRego(code)
}

func (e *Evaluator) Evaluate(ctx context.Context, code string, rawInput map[string]any) (EvalResult, error) {
	module, version, err := ParseAndValidate(code)
	if err != nil {
		return EvalResult{}, err
	}

	inputMap := normalizeInput(rawInput)
	pkgPath := module.Package.Path.String()

	r := rego.New(
		rego.Query(pkgPath),
		rego.ParsedModule(module),
		rego.Input(inputMap),
		rego.SetRegoVersion(version),
	)
	rs, err := r.Eval(ctx)
	if err != nil {
		return EvalResult{}, err
	}

	var pkgMap map[string]any
	if len(rs) > 0 && len(rs[0].Expressions) > 0 {
		if m, ok := rs[0].Expressions[0].Value.(map[string]any); ok {
			pkgMap = m
		}
	}

	deny := extractDenyMessages(pkgMap["deny"])
	allowVal, _ := pkgMap["allow"].(bool)

	hasAllowRule := false
	hasDenyRule := false
	for _, rule := range module.Rules {
		switch string(rule.Head.Name) {
		case "allow":
			hasAllowRule = true
		case "deny":
			hasDenyRule = true
		}
	}

	var allowed bool
	if hasAllowRule {
		allowed = allowVal && len(deny) == 0
	} else if hasDenyRule {
		allowed = len(deny) == 0
	} else {
		allowed = allowVal && len(deny) == 0
	}

	return EvalResult{
		Allowed: allowed,
		Deny:    deny,
	}, nil
}

func normalizeInput(rawInput map[string]any) map[string]any {
	inputMap := make(map[string]any, len(rawInput)+3)
	for k, v := range rawInput {
		inputMap[k] = v
	}

	planVal, hasPlan := inputMap["plan"]
	approvalsVal, hasApprovals := inputMap["approvals"]
	if nested, ok := inputMap["input"].(map[string]any); ok {
		nestedCopy := make(map[string]any, len(nested)+2)
		for k, v := range nested {
			nestedCopy[k] = v
		}
		if !hasPlan {
			if nestedPlan, exists := nestedCopy["plan"]; exists {
				inputMap["plan"] = nestedPlan
			}
		} else if _, exists := nestedCopy["plan"]; !exists {
			nestedCopy["plan"] = planVal
		}
		if !hasApprovals {
			if nestedApprovals, exists := nestedCopy["approvals"]; exists {
				approvalsVal = nestedApprovals
				hasApprovals = true
			}
		}
		if !hasApprovals || approvalsVal == nil {
			approvalsVal = []any{}
		}
		inputMap["approvals"] = approvalsVal
		nestedCopy["approvals"] = approvalsVal
		inputMap["input"] = nestedCopy
	} else if hasPlan {
		if !hasApprovals || approvalsVal == nil {
			approvalsVal = []any{}
		}
		inputMap["approvals"] = approvalsVal
		inputMap["input"] = map[string]any{"plan": planVal, "approvals": approvalsVal}
	} else {
		emptyPlan := map[string]any{}
		if !hasApprovals || approvalsVal == nil {
			approvalsVal = []any{}
		}
		inputMap["plan"] = emptyPlan
		inputMap["approvals"] = approvalsVal
		inputMap["input"] = map[string]any{"plan": emptyPlan, "approvals": approvalsVal}
	}
	if _, hasReviews := inputMap["reviews"]; !hasReviews {
		inputMap["reviews"] = map[string]any{"approvals": inputMap["approvals"]}
	}

	return inputMap
}

func extractDenyMessages(raw any) []string {
	if raw == nil {
		return []string{}
	}
	seen := make(map[string]struct{})
	addMsg := func(msg string) {
		msg = strings.TrimSpace(msg)
		if msg == "" {
			return
		}
		seen[msg] = struct{}{}
	}

	switch v := raw.(type) {
	case []string:
		for _, s := range v {
			addMsg(s)
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				addMsg(s)
			} else if item != nil {
				addMsg(fmt.Sprint(item))
			}
		}
	case map[string]any:
		for k, val := range v {
			if b, ok := val.(bool); !ok || b {
				addMsg(k)
			}
		}
	case string:
		addMsg(v)
	case bool:
		if v {
			addMsg("denied by policy")
		}
	}

	if len(seen) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(seen))
	for msg := range seen {
		out = append(out, msg)
	}
	sort.Strings(out)
	return out
}
