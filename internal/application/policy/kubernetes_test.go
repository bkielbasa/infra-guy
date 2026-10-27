package policyapp

import "testing"

func TestKubernetesObjectsFromManifest(t *testing.T) {
	plan := []byte(`{
		"resource_changes": [{
			"address": "Deployment.default.web",
			"change": {"actions": ["create"], "after": {"manifest": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: default\nspec:\n  template:\n    spec:\n      containers:\n      - name: web\n        image: nginx:1.25\n"}}
		}]
	}`)
	input, pretty, err := (&Service{}).BuildInput(plan, UserInfo{Login: "john"}, ProjectInfo{Name: "payments"})
	if err != nil {
		t.Fatal(err)
	}
	objects := objectsFrom(t, input["kubernetes"])
	if len(objects) != 1 {
		t.Fatalf("objects = %#v", objects)
	}
	obj := objects[0]
	if obj["kind"] != "Deployment" || obj["namespace"] != "default" || obj["name"] != "web" || obj["apiVersion"] != "apps/v1" {
		t.Fatalf("object = %#v", obj)
	}
	images, _ := obj["images"].([]string)
	if len(images) != 1 || images[0] != "nginx:1.25" {
		t.Fatalf("images = %#v", obj["images"])
	}
	nested, _ := input["input"].(map[string]any)
	if len(objectsFrom(t, nested["kubernetes"])) != 1 {
		t.Fatalf("nested input missing kubernetes objects: %#v", nested)
	}
	if pretty == "" {
		t.Fatal("expected pretty JSON")
	}
}

func objectsFrom(t *testing.T, raw any) []map[string]any {
	t.Helper()
	k8s, _ := raw.(map[string]any)
	objects, _ := k8s["objects"].([]map[string]any)
	return objects
}
