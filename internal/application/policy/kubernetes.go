package policyapp

import (
	"regexp"
	"sort"
	"strings"
)

var (
	yamlKindLine      = regexp.MustCompile(`(?m)^kind:\s*['"]?([A-Za-z0-9.]+)['"]?\s*$`)
	yamlAPILine       = regexp.MustCompile(`(?m)^apiVersion:\s*['"]?([A-Za-z0-9./]+)['"]?\s*$`)
	yamlNameLine      = regexp.MustCompile(`(?m)^[ \t]+name:\s*['"]?([^'"\s]+)['"]?\s*$`)
	yamlNamespaceLine = regexp.MustCompile(`(?m)^[ \t]+namespace:\s*['"]?([^'"\s]+)['"]?\s*$`)
	yamlImageLine     = regexp.MustCompile(`(?m)^[ \t]+image:\s*['"]?([^'"\s]+)['"]?\s*$`)
)

func kubernetesObjectsFromPlan(plan any) []map[string]any {
	root, ok := plan.(map[string]any)
	if !ok {
		return []map[string]any{}
	}
	changes, _ := root["resource_changes"].([]any)
	seen := map[string]bool{}
	var out []map[string]any
	for _, raw := range changes {
		rc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		change, _ := rc["change"].(map[string]any)
		for _, manifest := range manifestTexts(change) {
			for _, obj := range parseManifestObjects(manifest) {
				key := obj["apiVersion"].(string) + "/" + obj["kind"].(string) + "/" + obj["namespace"].(string) + "/" + obj["name"].(string)
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, obj)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i]["kind"] != out[j]["kind"] {
			return out[i]["kind"].(string) < out[j]["kind"].(string)
		}
		if out[i]["namespace"] != out[j]["namespace"] {
			return out[i]["namespace"].(string) < out[j]["namespace"].(string)
		}
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	if out == nil {
		return []map[string]any{}
	}
	return out
}

func manifestTexts(change map[string]any) []string {
	if change == nil {
		return nil
	}
	var texts []string
	for _, side := range []string{"after", "before"} {
		switch v := change[side].(type) {
		case map[string]any:
			if manifest, ok := v["manifest"].(string); ok && strings.TrimSpace(manifest) != "" {
				texts = append(texts, manifest)
			}
			if _, ok := v["kind"]; ok {
				if rendered := mapObjectManifest(v); rendered != "" {
					texts = append(texts, rendered)
				}
			}
		case string:
			if strings.TrimSpace(v) != "" {
				texts = append(texts, v)
			}
		}
	}
	return texts
}

func mapObjectManifest(obj map[string]any) string {
	kind, _ := obj["kind"].(string)
	name, _ := obj["name"].(string)
	if kind == "" || name == "" {
		return ""
	}
	api, _ := obj["apiVersion"].(string)
	ns, _ := obj["namespace"].(string)
	var b strings.Builder
	if api != "" {
		b.WriteString("apiVersion: " + api + "\n")
	}
	b.WriteString("kind: " + kind + "\n")
	b.WriteString("metadata:\n  name: " + name + "\n")
	if ns != "" {
		b.WriteString("  namespace: " + ns + "\n")
	}
	return b.String()
}

func parseManifestObjects(raw string) []map[string]any {
	var out []map[string]any
	for _, doc := range strings.Split(raw, "\n---") {
		doc = strings.TrimSpace(doc)
		if doc == "" || doc == "---" {
			continue
		}
		kind := firstMatch(yamlKindLine, doc)
		name := firstMatch(yamlNameLine, doc)
		if kind == "" || name == "" {
			continue
		}
		images := yamlImageLine.FindAllStringSubmatch(doc, -1)
		imageList := make([]string, 0, len(images))
		seenImg := map[string]bool{}
		for _, m := range images {
			if len(m) < 2 || seenImg[m[1]] {
				continue
			}
			seenImg[m[1]] = true
			imageList = append(imageList, m[1])
		}
		out = append(out, map[string]any{
			"apiVersion": firstMatch(yamlAPILine, doc),
			"kind":       kind,
			"namespace":  firstMatch(yamlNamespaceLine, doc),
			"name":       name,
			"images":     imageList,
		})
	}
	return out
}

func firstMatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
