package run

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// CostEstimate summarizes the estimated monthly cloud infrastructure cost
// before and after a Terraform plan, along with per-resource cost deltas.
type CostEstimate struct {
	PrevMonthlyCost float64        `json:"prev_monthly_cost"`
	MonthlyCost     float64        `json:"monthly_cost"`
	DiffMonthlyCost float64        `json:"diff_monthly_cost"`
	Currency        string         `json:"currency"`
	Resources       []ResourceCost `json:"resources,omitempty"`
}

// ResourceCost represents the estimated monthly cost impact of a single
// planned Terraform resource change.
type ResourceCost struct {
	Address         string  `json:"address"`
	ResourceType    string  `json:"resource_type"`
	Action          string  `json:"action"`
	SKU             string  `json:"sku,omitempty"`
	PrevMonthlyCost float64 `json:"prev_monthly_cost"`
	MonthlyCost     float64 `json:"monthly_cost"`
	DiffMonthlyCost float64 `json:"diff_monthly_cost"`
}

func (c *CostEstimate) HasCost() bool {
	if c == nil {
		return false
	}
	return c.PrevMonthlyCost > 0 || c.MonthlyCost > 0 || c.DiffMonthlyCost != 0 || len(c.Resources) > 0
}

func (c *CostEstimate) FormattedMonthly() string {
	if c == nil {
		return "$0.00/mo"
	}
	return FormatMonthlyCost(c.MonthlyCost)
}

func (c *CostEstimate) FormattedPrevMonthly() string {
	if c == nil {
		return "$0.00/mo"
	}
	return FormatMonthlyCost(c.PrevMonthlyCost)
}

func (c *CostEstimate) FormattedDiff() string {
	if c == nil {
		return "$0.00/mo"
	}
	return FormatCostDiff(c.DiffMonthlyCost)
}

func (r ResourceCost) FormattedMonthly() string {
	return FormatMonthlyCost(r.MonthlyCost)
}

func (r ResourceCost) FormattedPrevMonthly() string {
	return FormatMonthlyCost(r.PrevMonthlyCost)
}

func (r ResourceCost) FormattedDiff() string {
	return FormatCostDiff(r.DiffMonthlyCost)
}

func FormatMonthlyCost(amount float64) string {
	return fmt.Sprintf("$%.2f/mo", amount)
}

func FormatCostDiff(diff float64) string {
	if math.Abs(diff) < 0.005 {
		return "$0.00/mo"
	}
	if diff > 0 {
		return fmt.Sprintf("+$%.2f/mo", diff)
	}
	return fmt.Sprintf("-$%.2f/mo", math.Abs(diff))
}

// EstimatePlanCost parses raw `terraform show -json` bytes and computes
// the estimated monthly cost and delta across all resource changes.
func EstimatePlanCost(raw []byte) *CostEstimate {
	if len(raw) == 0 {
		return nil
	}
	var p rawPlanJSON
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	est := &CostEstimate{Currency: "USD"}
	for _, rc := range p.ResourceChanges {
		if rc.Mode == "data" {
			continue
		}
		action, _, _ := classifyActions(rc.Change.Actions)
		beforeMap, _ := rc.Change.Before.(map[string]any)
		afterMap, _ := rc.Change.After.(map[string]any)
		var prevCost, nextCost float64
		var prevSKU, nextSKU string

		switch action {
		case "create":
			nextCost, nextSKU = estimateResourceMonthlyCost(rc.Type, afterMap)
		case "delete":
			prevCost, prevSKU = estimateResourceMonthlyCost(rc.Type, beforeMap)
		case "update", "replace":
			prevCost, prevSKU = estimateResourceMonthlyCost(rc.Type, beforeMap)
			nextCost, nextSKU = estimateResourceMonthlyCost(rc.Type, afterMap)
		default:
			if afterMap != nil {
				c, sku := estimateResourceMonthlyCost(rc.Type, afterMap)
				prevCost, nextCost, nextSKU = c, c, sku
			} else if beforeMap != nil {
				c, sku := estimateResourceMonthlyCost(rc.Type, beforeMap)
				prevCost, nextCost, prevSKU = c, c, sku
			}
		}

		prevCost = roundCents(prevCost)
		nextCost = roundCents(nextCost)
		diff := roundCents(nextCost - prevCost)

		est.PrevMonthlyCost = roundCents(est.PrevMonthlyCost + prevCost)
		est.MonthlyCost = roundCents(est.MonthlyCost + nextCost)
		est.DiffMonthlyCost = roundCents(est.MonthlyCost - est.PrevMonthlyCost)

		if prevCost == 0 && nextCost == 0 {
			continue
		}
		sku := nextSKU
		if sku == "" {
			sku = prevSKU
		} else if prevSKU != "" && prevSKU != nextSKU {
			sku = prevSKU + " → " + nextSKU
		}
		addr := rc.Address
		if addr == "" && rc.Type != "" && rc.Name != "" {
			addr = rc.Type + "." + rc.Name
		}
		est.Resources = append(est.Resources, ResourceCost{
			Address:         addr,
			ResourceType:    rc.Type,
			Action:          action,
			SKU:             sku,
			PrevMonthlyCost: prevCost,
			MonthlyCost:     nextCost,
			DiffMonthlyCost: diff,
		})
	}
	return est
}

func roundCents(v float64) float64 {
	return math.Round(v*100) / 100
}

var awsEC2MonthlyPricing = map[string]float64{
	"t2.nano":     4.23,
	"t2.micro":    8.47,
	"t2.small":    16.79,
	"t2.medium":   33.87,
	"t2.large":    67.74,
	"t3.nano":     3.80,
	"t3.micro":    7.59,
	"t3.small":    15.18,
	"t3.medium":   30.37,
	"t3.large":    60.74,
	"t3.xlarge":   121.47,
	"t3.2xlarge":  242.94,
	"t3a.micro":   6.86,
	"t3a.small":   13.72,
	"t3a.medium":  27.45,
	"t3a.large":   54.90,
	"t4g.nano":    3.07,
	"t4g.micro":   6.13,
	"t4g.small":   12.26,
	"t4g.medium":  24.53,
	"t4g.large":   49.06,
	"m5.large":    70.08,
	"m5.xlarge":   140.16,
	"m5.2xlarge":  280.32,
	"m5.4xlarge":  560.64,
	"m6i.large":   70.08,
	"m6i.xlarge":  140.16,
	"m6i.2xlarge": 280.32,
	"m6g.large":   56.21,
	"m6g.xlarge":  112.42,
	"c5.large":    62.05,
	"c5.xlarge":   124.10,
	"c5.2xlarge":  248.20,
	"c6i.large":   62.05,
	"c6i.xlarge":  124.10,
	"r5.large":    91.98,
	"r5.xlarge":   183.96,
	"r5.2xlarge":  367.92,
	"r6i.large":   91.98,
	"r6i.xlarge":  183.96,
}

var awsRDSMonthlyPricing = map[string]float64{
	"db.t3.micro":    12.41,
	"db.t3.small":    24.82,
	"db.t3.medium":   49.64,
	"db.t3.large":    99.28,
	"db.t3.xlarge":   198.56,
	"db.t4g.micro":   11.68,
	"db.t4g.small":   23.36,
	"db.t4g.medium":  46.72,
	"db.t4g.large":   93.44,
	"db.m5.large":    124.83,
	"db.m5.xlarge":   249.66,
	"db.m5.2xlarge":  499.32,
	"db.m6i.large":   124.83,
	"db.m6i.xlarge":  249.66,
	"db.r5.large":    175.20,
	"db.r5.xlarge":   350.40,
	"db.r5.2xlarge":  700.80,
	"db.r6g.large":   156.95,
	"db.r6g.xlarge":  313.90,
}

var awsElastiCacheMonthlyPricing = map[string]float64{
	"cache.t3.micro":   12.41,
	"cache.t3.small":   24.82,
	"cache.t3.medium":  49.64,
	"cache.t4g.micro":  11.68,
	"cache.t4g.small":  23.36,
	"cache.t4g.medium": 46.72,
	"cache.m5.large":   113.88,
	"cache.m5.xlarge":  227.76,
	"cache.r5.large":   148.92,
}

var gcpComputeMonthlyPricing = map[string]float64{
	"f1-micro":      4.28,
	"g1-small":      13.88,
	"e2-micro":      6.11,
	"e2-small":      12.23,
	"e2-medium":     24.46,
	"e2-standard-2": 48.91,
	"e2-standard-4": 97.82,
	"e2-standard-8": 195.64,
	"n1-standard-1": 24.27,
	"n1-standard-2": 48.55,
	"n1-standard-4": 97.09,
	"n2-standard-2": 56.59,
	"n2-standard-4": 113.18,
	"n2-standard-8": 226.36,
}

var azureVMMonthlyPricing = map[string]float64{
	"standard_b1s":    7.59,
	"standard_b1ms":   15.18,
	"standard_b2s":    30.37,
	"standard_b2ms":   60.74,
	"standard_d2s_v3": 70.08,
	"standard_d4s_v3": 140.16,
	"standard_d8s_v3": 280.32,
	"standard_d2s_v5": 70.08,
	"standard_d4s_v5": 140.16,
	"standard_e2s_v3": 91.98,
	"standard_e4s_v3": 183.96,
}

func estimateResourceMonthlyCost(resType string, attrs map[string]any) (float64, string) {
	if attrs == nil {
		attrs = map[string]any{}
	}
	switch resType {
	case "aws_instance", "aws_launch_template":
		it := strings.ToLower(strings.TrimSpace(attrString(attrs, "instance_type")))
		if it == "" {
			it = "t3.medium"
		}
		base, ok := awsEC2MonthlyPricing[it]
		if !ok {
			base = 30.37
		}
		return base, it

	case "aws_db_instance", "aws_rds_cluster_instance":
		ic := strings.ToLower(strings.TrimSpace(attrString(attrs, "instance_class")))
		if ic == "" {
			ic = "db.t3.medium"
		}
		base, ok := awsRDSMonthlyPricing[ic]
		if !ok {
			base = 49.64
		}
		if attrBool(attrs, "multi_az") {
			base *= 2
			ic += " (multi-az)"
		}
		storageGB := attrFloat(attrs, "allocated_storage", 20)
		return base + storageGB*0.115, ic

	case "aws_nat_gateway":
		return 32.85, "nat-gateway"

	case "aws_lb", "aws_alb", "aws_elb":
		lbType := strings.ToLower(attrString(attrs, "load_balancer_type"))
		if lbType == "" {
			lbType = "application"
		}
		return 16.43, lbType

	case "aws_eks_cluster":
		return 73.00, "eks-control-plane"

	case "aws_eks_node_group":
		it := "t3.medium"
		if types, ok := attrs["instance_types"].([]any); ok && len(types) > 0 {
			if s, ok := types[0].(string); ok && s != "" {
				it = strings.ToLower(s)
			}
		}
		perNode, ok := awsEC2MonthlyPricing[it]
		if !ok {
			perNode = 30.37
		}
		desired := 2.0
		if sc, ok := attrs["scaling_config"].([]any); ok && len(sc) > 0 {
			if m, ok := sc[0].(map[string]any); ok {
				desired = attrFloat(m, "desired_size", 2)
			}
		}
		return perNode * desired, fmt.Sprintf("%dx %s", int(desired), it)

	case "aws_ebs_volume":
		size := attrFloat(attrs, "size", 20)
		volType := strings.ToLower(attrString(attrs, "type"))
		if volType == "" {
			volType = "gp3"
		}
		rate := 0.08
		if volType == "io1" || volType == "io2" {
			rate = 0.125
		}
		return size * rate, fmt.Sprintf("%gGB %s", size, volType)

	case "aws_elasticache_cluster", "aws_elasticache_replication_group":
		nt := strings.ToLower(attrString(attrs, "node_type"))
		if nt == "" {
			nt = "cache.t3.small"
		}
		base, ok := awsElastiCacheMonthlyPricing[nt]
		if !ok {
			base = 24.82
		}
		nodes := attrFloat(attrs, "num_cache_nodes", 0)
		if nodes <= 0 {
			nodes = attrFloat(attrs, "num_cache_clusters", 1)
		}
		return base * nodes, nt

	case "aws_ecs_service":
		desired := attrFloat(attrs, "desired_count", 1)
		if desired <= 0 {
			desired = 1
		}
		return desired * 18.50, fmt.Sprintf("%d tasks", int(desired))

	case "aws_lambda_function":
		mem := attrFloat(attrs, "memory_size", 128)
		return math.Max(5.00, (mem/128.0)*2.50), fmt.Sprintf("%dMB", int(mem))

	case "aws_s3_bucket":
		return 2.30, "s3-standard"

	case "aws_cloudfront_distribution":
		return 8.50, "cloudfront"

	case "aws_kms_key":
		return 1.00, "kms-key"

	case "aws_secretsmanager_secret":
		return 0.40, "secret"

	case "aws_eip":
		return 3.65, "elastic-ip"

	case "aws_vpn_connection":
		return 36.50, "site-to-site-vpn"

	case "google_compute_instance":
		mt := strings.ToLower(attrString(attrs, "machine_type"))
		if idx := strings.LastIndex(mt, "/"); idx >= 0 {
			mt = mt[idx+1:]
		}
		if mt == "" {
			mt = "e2-medium"
		}
		base, ok := gcpComputeMonthlyPricing[mt]
		if !ok {
			base = 24.46
		}
		return base, mt

	case "google_sql_database_instance":
		tier := "db-custom-1-3840"
		if settings, ok := attrs["settings"].([]any); ok && len(settings) > 0 {
			if m, ok := settings[0].(map[string]any); ok {
				if t := attrString(m, "tier"); t != "" {
					tier = strings.ToLower(t)
				}
			}
		}
		switch tier {
		case "db-f1-micro":
			return 7.67, tier
		case "db-g1-small":
			return 25.55, tier
		default:
			return 45.00, tier
		}

	case "google_container_cluster":
		return 73.00, "gke-cluster"

	case "google_compute_disk":
		size := attrFloat(attrs, "size", 20)
		return size * 0.10, fmt.Sprintf("%gGB pd", size)

	case "google_storage_bucket":
		return 2.00, "gcs-bucket"

	case "azurerm_linux_virtual_machine", "azurerm_windows_virtual_machine", "azurerm_virtual_machine":
		sz := strings.ToLower(attrString(attrs, "size"))
		if sz == "" {
			sz = strings.ToLower(attrString(attrs, "vm_size"))
		}
		if sz == "" {
			sz = "standard_d2s_v3"
		}
		base, ok := azureVMMonthlyPricing[sz]
		if !ok {
			base = 70.08
		}
		return base, sz

	case "azurerm_kubernetes_cluster":
		return 73.00, "aks-cluster"

	case "azurerm_mssql_database", "azurerm_postgresql_flexible_server", "azurerm_mysql_flexible_server":
		return 48.00, "managed-db"

	case "azurerm_managed_disk":
		size := attrFloat(attrs, "disk_size_gb", 32)
		return size * 0.095, fmt.Sprintf("%gGB disk", size)

	case "azurerm_lb", "azurerm_application_gateway":
		return 18.25, "azure-lb"
	}
	return 0, ""
}

func attrString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func attrBool(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	if s, ok := v.(string); ok {
		return strings.EqualFold(s, "true")
	}
	return false
}

func attrFloat(m map[string]any, key string, fallback float64) float64 {
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f
		}
	}
	return fallback
}
