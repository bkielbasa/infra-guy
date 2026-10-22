package run

import (
	"testing"
	"time"
)

func TestParsePlanJSONAndSummary(t *testing.T) {
	raw := []byte(`{
		"format_version": "1.2",
		"terraform_version": "1.9.8",
		"resource_changes": [
			{
				"address": "aws_s3_bucket.logs",
				"mode": "managed",
				"type": "aws_s3_bucket",
				"name": "logs",
				"provider_name": "registry.terraform.io/hashicorp/aws",
				"change": {
					"actions": ["create"],
					"before": null,
					"after": {"bucket": "my-logs-bucket", "force_destroy": false, "tags": null},
					"after_unknown": {"arn": true, "id": true},
					"before_sensitive": false,
					"after_sensitive": {}
				}
			},
			{
				"address": "aws_iam_User.app",
				"mode": "managed",
				"type": "aws_iam_user",
				"name": "app",
				"provider_name": "registry.terraform.io/hashicorp/aws",
				"change": {
					"actions": ["update"],
					"before": {"name": "app-old", "path": "/system/", "secret_token": "old-sec"},
					"after": {"name": "app-new", "path": "/system/", "secret_token": "new-sec"},
					"after_unknown": {},
					"before_sensitive": {"secret_token": true},
					"after_sensitive": {"secret_token": true}
				}
			},
			{
				"address": "aws_instance.worker",
				"mode": "managed",
				"type": "aws_instance",
				"name": "worker",
				"provider_name": "registry.terraform.io/hashicorp/aws",
				"action_reason": "replace_because_cannot_update",
				"change": {
					"actions": ["delete", "create"],
					"before": {"ami": "ami-111", "instance_type": "t3.micro"},
					"after": {"ami": "ami-222", "instance_type": "t3.micro"},
					"after_unknown": {"public_ip": true},
					"replace_paths": [["ami"]]
				}
			},
			{
				"address": "aws_security_group.legacy",
				"mode": "managed",
				"type": "aws_security_group",
				"name": "legacy",
				"change": {
					"actions": ["delete"],
					"before": {"name": "legacy-sg"},
					"after": null
				}
			},
			{
				"address": "aws_vpc.main",
				"mode": "managed",
				"type": "aws_vpc",
				"name": "main",
				"change": {
					"actions": ["no-op"],
					"before": {"cidr_block": "10.0.0.0/16"},
					"after": {"cidr_block": "10.0.0.0/16"}
				}
			}
		],
		"output_changes": {
			"bucket_arn": {
				"actions": ["create"],
				"before": null,
				"after": null,
				"after_unknown": true,
				"before_sensitive": false,
				"after_sensitive": false
			},
			"db_password": {
				"actions": ["update"],
				"before": "pw1",
				"after": "pw2",
				"after_unknown": false,
				"before_sensitive": true,
				"after_sensitive": true
			}
		}
	}`)

	parsed, err := ParsePlanJSON(raw)
	if err != nil {
		t.Fatalf("ParsePlanJSON error: %v", err)
	}
	if parsed == nil || !parsed.HasChanges() {
		t.Fatalf("expected non-empty ParsedPlan, got %#v", parsed)
	}
	if parsed.Add != 1 || parsed.Change != 1 || parsed.Replace != 1 || parsed.Destroy != 1 {
		t.Fatalf("counts = +%d ~%d -/+%d -%d", parsed.Add, parsed.Change, parsed.Replace, parsed.Destroy)
	}
	if len(parsed.Resources) != 4 {
		t.Fatalf("expected 4 changed resources (no-op filtered), got %d", len(parsed.Resources))
	}

	// 1. Create resource
	createRes := parsed.Resources[0]
	if createRes.Address != "aws_s3_bucket.logs" || createRes.Action != "create" || createRes.ProviderName != "aws" {
		t.Fatalf("unexpected create resource: %+v", createRes)
	}
	// Should include arn (known after apply), bucket, force_destroy, id (known after apply), and skip null tags
	if len(createRes.Attributes) != 4 {
		t.Fatalf("expected 4 create attributes, got %+v", createRes.Attributes)
	}
	if !createRes.Attributes[0].KnownAfterApply || createRes.Attributes[0].After != "(known after apply)" {
		t.Fatalf("expected arn to be known after apply, got %+v", createRes.Attributes[0])
	}

	// 2. Update resource with sensitive attribute and unchanged attribute
	updateRes := parsed.Resources[1]
	if updateRes.Action != "update" || updateRes.UnchangedCount != 1 {
		t.Fatalf("expected 1 unchanged attribute on updateRes, got %+v", updateRes)
	}
	var foundSens bool
	for _, a := range updateRes.Attributes {
		if a.Name == "secret_token" {
			foundSens = true
			if !a.Sensitive || a.Before != "(sensitive value)" || a.After != "(sensitive value)" {
				t.Fatalf("sensitive attribute leaked value: %+v", a)
			}
		}
	}
	if !foundSens {
		t.Fatalf("expected secret_token in changed attributes: %+v", updateRes.Attributes)
	}

	// 3. Replace resource with replace_paths and action_reason
	replaceRes := parsed.Resources[2]
	if replaceRes.Action != "replace" || replaceRes.ActionSymbol != "-/+" || replaceRes.ActionReason != "forces replacement" {
		t.Fatalf("unexpected replace resource: %+v", replaceRes)
	}
	if len(replaceRes.Attributes) == 0 || replaceRes.Attributes[0].Name != "ami" || !replaceRes.Attributes[0].ForcesReplace {
		t.Fatalf("expected ami attribute to be marked ForcesReplace: %+v", replaceRes.Attributes)
	}

	// 4. Outputs
	if len(parsed.Outputs) != 2 {
		t.Fatalf("expected 2 outputs, got %+v", parsed.Outputs)
	}
	if !parsed.Outputs[0].KnownAfterApply || parsed.Outputs[0].After != "(known after apply)" {
		t.Fatalf("expected bucket_arn output known after apply: %+v", parsed.Outputs[0])
	}
	if !parsed.Outputs[1].Sensitive || parsed.Outputs[1].After != "(sensitive value)" {
		t.Fatalf("expected db_password output masked: %+v", parsed.Outputs[1])
	}

	// 5. Summary fallback when PlanText is empty
	r, err := Open(OpenInput{
		ID: "r1", ProjectID: "p1", SHA: "abc", Trigger: TriggerPush, Now: time.Unix(1, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.BeginStep(StepInit, time.Unix(2, 0).UTC())
	_, _ = r.CompleteStep(StepInit, StepResult{ExitCode: 0}, time.Unix(3, 0).UTC())
	_ = r.BeginStep(StepPlan, time.Unix(4, 0).UTC())
	if err := r.SetStepLogs(StepPlan, "streaming line 1\n"); err != nil {
		t.Fatal(err)
	}
	if r.Snapshot().Steps[1].Logs != "streaming line 1\n" {
		t.Fatalf("SetStepLogs = %q", r.Snapshot().Steps[1].Logs)
	}
	dec, err := r.CompleteStep(StepPlan, StepResult{
		ExitCode: 0,
		Logs:     "streaming line 1\ndone\n",
		PlanJSON: raw,
	}, time.Unix(5, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if dec.Kind != DecisionWaitApproval {
		t.Fatalf("decision = %+v, want DecisionWaitApproval", dec)
	}
	snap := r.Snapshot()
	// 1 create + 1 replace = 2 add; 1 update = 1 change; 1 delete + 1 replace = 2 destroy
	if !snap.SummaryPresent || snap.Add != 2 || snap.Change != 1 || snap.Destroy != 2 {
		t.Fatalf("snapshot summary = present:%v +%d ~%d -%d", snap.SummaryPresent, snap.Add, snap.Change, snap.Destroy)
	}
}
