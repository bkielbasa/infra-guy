package run

import (
	"testing"
)

func TestEstimatePlanCost(t *testing.T) {
	raw := []byte(`{
		"resource_changes": [
			{
				"address": "aws_instance.web",
				"mode": "managed",
				"type": "aws_instance",
				"name": "web",
				"change": {
					"actions": ["create"],
					"before": null,
					"after": {"instance_type": "t3.large"}
				}
			},
			{
				"address": "aws_instance.api",
				"mode": "managed",
				"type": "aws_instance",
				"name": "api",
				"change": {
					"actions": ["update"],
					"before": {"instance_type": "t3.small"},
					"after": {"instance_type": "t3.medium"}
				}
			},
			{
				"address": "aws_nat_gateway.old",
				"mode": "managed",
				"type": "aws_nat_gateway",
				"name": "old",
				"change": {
					"actions": ["delete"],
					"before": {"connectivity_type": "public"},
					"after": null
				}
			}
		]
	}`)

	cost := EstimatePlanCost(raw)
	if cost == nil {
		t.Fatal("expected non-nil CostEstimate")
	}
	// prev: t3.small (15.18) + nat_gateway (32.85) = 48.03
	if cost.PrevMonthlyCost != 48.03 {
		t.Fatalf("expected PrevMonthlyCost=48.03, got %v", cost.PrevMonthlyCost)
	}
	// next: t3.large (60.74) + t3.medium (30.37) = 91.11
	if cost.MonthlyCost != 91.11 {
		t.Fatalf("expected MonthlyCost=91.11, got %v", cost.MonthlyCost)
	}
	// diff: 91.11 - 48.03 = +43.08
	if cost.DiffMonthlyCost != 43.08 {
		t.Fatalf("expected DiffMonthlyCost=43.08, got %v", cost.DiffMonthlyCost)
	}
	if cost.FormattedDiff() != "+$43.08/mo" {
		t.Fatalf("expected FormattedDiff()=+$43.08/mo, got %q", cost.FormattedDiff())
	}
	if len(cost.Resources) != 3 {
		t.Fatalf("expected 3 resource cost items, got %d", len(cost.Resources))
	}
}
