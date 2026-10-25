package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type ParsedPlan struct {
	FormatVersion    string
	TerraformVersion string
	Add              int
	Change           int
	Replace          int
	Destroy          int
	Resources        []ResourceDiff
	Outputs          []OutputDiff
	Cost             *CostEstimate
}

func (p *ParsedPlan) HasChanges() bool {
	return p != nil && (len(p.Resources) > 0 || len(p.Outputs) > 0 || p.Add > 0 || p.Change > 0 || p.Replace > 0 || p.Destroy > 0)
}

type ResourceDiff struct {
	Address        string
	ModuleAddress  string
	Mode           string
	Type           string
	Name           string
	ProviderName   string
	Action         string
	ActionSymbol   string
	ActionLabel    string
	ActionReason   string
	CostDiff       string
	CostSKU        string
	Attributes     []AttributeDiff
	UnchangedCount int
	UnchangedAttrs []AttributeDiff
}

type AttributeDiff struct {
	Name            string
	Change          string
	Symbol          string
	Before          string
	After           string
	KnownAfterApply bool
	Sensitive       bool
	ForcesReplace   bool
}

type OutputDiff struct {
	Name            string
	Action          string
	Symbol          string
	Before          string
	After           string
	KnownAfterApply bool
	Sensitive       bool
}

type rawPlanJSON struct {
	FormatVersion    string                   `json:"format_version"`
	TerraformVersion string                   `json:"terraform_version"`
	ResourceChanges  []rawResourceChange      `json:"resource_changes"`
	OutputChanges    map[string]rawChangeSpec `json:"output_changes"`
}

type rawResourceChange struct {
	Address       string        `json:"address"`
	ModuleAddress string        `json:"module_address"`
	Mode          string        `json:"mode"`
	Type          string        `json:"type"`
	Name          string        `json:"name"`
	ProviderName  string        `json:"provider_name"`
	ActionReason  string        `json:"action_reason"`
	Change        rawChangeSpec `json:"change"`
}

type rawChangeSpec struct {
	Actions         []string `json:"actions"`
	Before          any      `json:"before"`
	After           any      `json:"after"`
	AfterUnknown    any      `json:"after_unknown"`
	BeforeSensitive any      `json:"before_sensitive"`
	AfterSensitive  any      `json:"after_sensitive"`
	ReplacePaths    []any    `json:"replace_paths"`
}

func ParsePlanSummaryJSON(raw []byte) (PlanSummary, bool) {
	parsed, err := ParsePlanJSON(raw)
	if err != nil || parsed == nil {
		return PlanSummary{}, false
	}
	add := parsed.Add + parsed.Replace
	change := parsed.Change
	destroy := parsed.Destroy + parsed.Replace
	if add == 0 && change == 0 && destroy == 0 && len(parsed.Outputs) == 0 {
		return PlanSummary{Present: false, noChangesSentence: true}, true
	}
	return PlanSummary{
		Present: true,
		Add:     add,
		Change:  change,
		Destroy: destroy,
	}, true
}

func ParsePlanJSON(raw []byte) (*ParsedPlan, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var doc rawPlanJSON
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return nil, err
	}
	out := &ParsedPlan{
		FormatVersion:    doc.FormatVersion,
		TerraformVersion: doc.TerraformVersion,
		Cost:             EstimatePlanCost(trimmed),
	}
	costByAddr := make(map[string]ResourceCost)
	if out.Cost != nil {
		for _, rc := range out.Cost.Resources {
			costByAddr[rc.Address] = rc
		}
	}
	for _, rc := range doc.ResourceChanges {
		action, sym, label := classifyActions(rc.Change.Actions)
		if action == "no-op" || action == "" {
			continue
		}
		switch action {
		case "create":
			out.Add++
		case "update":
			out.Change++
		case "replace":
			out.Replace++
		case "delete":
			out.Destroy++
		}
		changed, unchanged := buildAttributeDiffs(action, rc.Change)
		addr := rc.Address
		if addr == "" && rc.Type != "" && rc.Name != "" {
			addr = rc.Type + "." + rc.Name
		}
		var costDiff, costSKU string
		if c, ok := costByAddr[addr]; ok {
			costDiff = c.FormattedDiff()
			costSKU = c.SKU
		}
		out.Resources = append(out.Resources, ResourceDiff{
			Address:        addr,
			ModuleAddress:  rc.ModuleAddress,
			Mode:           rc.Mode,
			Type:           rc.Type,
			Name:           rc.Name,
			ProviderName:   shortProviderName(rc.ProviderName),
			Action:         action,
			ActionSymbol:   sym,
			ActionLabel:    label,
			ActionReason:   humanActionReason(rc.ActionReason),
			CostDiff:       costDiff,
			CostSKU:        costSKU,
			Attributes:     changed,
			UnchangedCount: len(unchanged),
			UnchangedAttrs: unchanged,
		})
	}
	if len(doc.OutputChanges) > 0 {
		names := make([]string, 0, len(doc.OutputChanges))
		for name := range doc.OutputChanges {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			oc := doc.OutputChanges[name]
			action, sym, _ := classifyActions(oc.Actions)
			if action == "no-op" || action == "" {
				continue
			}
			sens := isTruthy(oc.BeforeSensitive) || isTruthy(oc.AfterSensitive)
			unknown := isTruthy(oc.AfterUnknown)
			beforeStr := formatValue(oc.Before)
			afterStr := formatValue(oc.After)
			if sens {
				beforeStr = "(sensitive value)"
				afterStr = "(sensitive value)"
			} else if unknown {
				afterStr = "(known after apply)"
			}
			out.Outputs = append(out.Outputs, OutputDiff{
				Name:            name,
				Action:          action,
				Symbol:          sym,
				Before:          beforeStr,
				After:           afterStr,
				KnownAfterApply: unknown,
				Sensitive:       sens,
			})
		}
	}
	return out, nil
}

func classifyActions(actions []string) (action, symbol, label string) {
	if len(actions) == 0 {
		return "", "", ""
	}
	if len(actions) == 2 && ((actions[0] == "delete" && actions[1] == "create") || (actions[0] == "create" && actions[1] == "delete")) {
		return "replace", "-/+", "replace"
	}
	switch actions[0] {
	case "create":
		return "create", "+", "create"
	case "update":
		return "update", "~", "update"
	case "delete":
		return "delete", "-", "destroy"
	case "read":
		return "read", "<=", "read"
	case "no-op":
		return "no-op", "", "no-op"
	default:
		return actions[0], "~", actions[0]
	}
}

func buildAttributeDiffs(action string, ch rawChangeSpec) (changed []AttributeDiff, unchanged []AttributeDiff) {
	beforeMap, _ := ch.Before.(map[string]any)
	afterMap, _ := ch.After.(map[string]any)
	unknownMap, _ := ch.AfterUnknown.(map[string]any)
	beforeSensMap, _ := ch.BeforeSensitive.(map[string]any)
	afterSensMap, _ := ch.AfterSensitive.(map[string]any)
	replaceKeys := extractReplaceTopKeys(ch.ReplacePaths)

	keySet := make(map[string]struct{})
	for k := range beforeMap {
		keySet[k] = struct{}{}
	}
	for k := range afterMap {
		keySet[k] = struct{}{}
	}
	for k := range unknownMap {
		keySet[k] = struct{}{}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		bVal, hasBefore := beforeMap[k]
		aVal, hasAfter := afterMap[k]
		uVal := unknownMap[k]
		knownAfter := isTruthy(uVal)
		sens := isTruthy(beforeSensMap[k]) || isTruthy(afterSensMap[k])
		forcesReplace := replaceKeys[k]

		if sens {
			diff := AttributeDiff{
				Name:          k,
				Before:        "(sensitive value)",
				After:         "(sensitive value)",
				Sensitive:     true,
				ForcesReplace: forcesReplace,
			}
			switch action {
			case "create":
				diff.Change = "add"
				diff.Symbol = "+"
				changed = append(changed, diff)
			case "delete":
				diff.Change = "delete"
				diff.Symbol = "-"
				changed = append(changed, diff)
			default:
				if !reflect.DeepEqual(bVal, aVal) || knownAfter {
					diff.Change = "update"
					diff.Symbol = "~"
					changed = append(changed, diff)
				} else {
					diff.Change = "unchanged"
					diff.Symbol = " "
					unchanged = append(unchanged, diff)
				}
			}
			continue
		}

		switch action {
		case "create":
			if !knownAfter && (!hasAfter || isEmptyValue(aVal)) {
				continue
			}
			afterStr := formatValue(aVal)
			if knownAfter {
				afterStr = "(known after apply)"
			}
			changed = append(changed, AttributeDiff{
				Name:            k,
				Change:          "add",
				Symbol:          "+",
				After:           afterStr,
				KnownAfterApply: knownAfter,
			})
		case "delete":
			if !hasBefore || isEmptyValue(bVal) {
				continue
			}
			changed = append(changed, AttributeDiff{
				Name:   k,
				Change: "delete",
				Symbol: "-",
				Before: formatValue(bVal),
			})
		default:
			if knownAfter {
				changed = append(changed, AttributeDiff{
					Name:            k,
					Change:          "update",
					Symbol:          "~",
					Before:          formatValue(bVal),
					After:           "(known after apply)",
					KnownAfterApply: true,
					ForcesReplace:   forcesReplace,
				})
				continue
			}
			if !reflect.DeepEqual(bVal, aVal) {
				if !hasBefore || bVal == nil {
					changed = append(changed, AttributeDiff{
						Name:          k,
						Change:        "add",
						Symbol:        "+",
						After:         formatValue(aVal),
						ForcesReplace: forcesReplace,
					})
				} else if !hasAfter || aVal == nil {
					changed = append(changed, AttributeDiff{
						Name:          k,
						Change:        "delete",
						Symbol:        "-",
						Before:        formatValue(bVal),
						ForcesReplace: forcesReplace,
					})
				} else {
					changed = append(changed, AttributeDiff{
						Name:          k,
						Change:        "update",
						Symbol:        "~",
						Before:        formatValue(bVal),
						After:         formatValue(aVal),
						ForcesReplace: forcesReplace,
					})
				}
				continue
			}
			if isEmptyValue(aVal) {
				continue
			}
			unchanged = append(unchanged, AttributeDiff{
				Name:   k,
				Change: "unchanged",
				Symbol: " ",
				After:  formatValue(aVal),
			})
		}
	}
	return changed, unchanged
}

func extractReplaceTopKeys(paths []any) map[string]bool {
	out := make(map[string]bool)
	for _, p := range paths {
		arr, ok := p.([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		if s, ok := arr[0].(string); ok && s != "" {
			out[s] = true
		}
	}
	return out
}

func isTruthy(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case map[string]any:
		for _, child := range val {
			if isTruthy(child) {
				return true
			}
		}
	case []any:
		for _, child := range val {
			if isTruthy(child) {
				return true
			}
		}
	}
	return false
}

func isEmptyValue(v any) bool {
	if v == nil {
		return true
	}
	switch val := v.(type) {
	case []any:
		return len(val) == 0
	case map[string]any:
		return len(val) == 0
	}
	return false
}

func formatValue(v any) string {
	if v == nil {
		return "null"
	}
	switch val := v.(type) {
	case string:
		return fmt.Sprintf("%q", val)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case float64:
		b, _ := json.Marshal(val)
		return string(b)
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}
}

func shortProviderName(p string) string {
	if idx := strings.LastIndex(p, "/"); idx >= 0 && idx+1 < len(p) {
		return p[idx+1:]
	}
	return p
}

func humanActionReason(reason string) string {
	switch reason {
	case "replace_because_cannot_update":
		return "forces replacement"
	case "replace_because_tainted":
		return "tainted"
	case "replace_by_request":
		return "requested replacement"
	case "replace_because_ers":
		return "triggered replacement"
	default:
		return strings.ReplaceAll(reason, "_", " ")
	}
}
