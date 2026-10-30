package folder

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"infra-guy/internal/domain/project"
)

type Snapshot struct {
	ID, TenantID, ParentID, Name string
	PassDown, TagsSet            bool
	Tags                         map[string]string
	PolicyIDs                    []string
	CreatedAt, UpdatedAt         time.Time
}

func ParseName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("folder name is required")
	}
	if strings.Contains(name, "/") {
		return "", errors.New("folder name cannot contain a slash")
	}
	for _, r := range name {
		if r < ' ' {
			return "", errors.New("folder name contains invalid characters")
		}
	}
	if len(name) > 80 {
		return "", errors.New("folder name is too long")
	}
	return name, nil
}

func ValidateMove(folders []Snapshot, id, newParentID string) error {
	byID, children := indexFolders(folders)
	moved, ok := byID[id]
	if !ok {
		return fmt.Errorf("folder %s not found", id)
	}
	newParentID = strings.TrimSpace(newParentID)
	if moved.ParentID == "" || newParentID == "" {
		return errors.New("the root folder cannot be moved")
	}
	seen := map[string]bool{}
	parentDepth := 0
	for cur := newParentID; cur != ""; {
		if cur == id {
			return errors.New("folder cannot be moved under itself")
		}
		if seen[cur] {
			return fmt.Errorf("folder %s not found", cur)
		}
		seen[cur] = true
		parent, ok := byID[cur]
		if !ok {
			return fmt.Errorf("folder %s not found", cur)
		}
		parentDepth++
		cur = parent.ParentID
	}
	if parentDepth+1+subtreeExtra(children, id) > 8 {
		return errors.New("folder depth cannot exceed 8")
	}
	return nil
}

func Resolve(p project.Snapshot, folders []Snapshot) (project.Snapshot, error) {
	byID, _ := indexFolders(folders)
	chain := make([]Snapshot, 0)
	seen := map[string]bool{}
	for id := p.FolderID; id != ""; {
		if seen[id] {
			return project.Snapshot{}, fmt.Errorf("folder %s not found", id)
		}
		seen[id] = true
		f, ok := byID[id]
		if !ok {
			return project.Snapshot{}, fmt.Errorf("folder %s not found", id)
		}
		chain = append(chain, f)
		id = f.ParentID
	}
	if len(chain) == 0 {
		return project.Snapshot{}, fmt.Errorf("folder %s not found", p.FolderID)
	}

	out := p
	out.Tags = cloneTags(p.Tags)
	out.PolicyIDs = cloneStrings(p.PolicyIDs)
	if p.TagsOverridden {
		out.EffectiveTags = copyTags(p.Tags)
	} else {
		out.EffectiveTags = map[string]string{}
		for i, f := range chain {
			if f.TagsSet && (i == 0 || f.PassDown) {
				out.EffectiveTags = copyTags(f.Tags)
				break
			}
		}
	}

	seenPol := map[string]bool{}
	var policies []project.EffectivePolicy
	add := func(ids []string, source string) {
		sorted := cloneStrings(ids)
		sort.Strings(sorted)
		for _, id := range sorted {
			if seenPol[id] {
				continue
			}
			seenPol[id] = true
			policies = append(policies, project.EffectivePolicy{PolicyID: id, Source: source})
		}
	}
	add(p.PolicyIDs, "project")
	add(chain[0].PolicyIDs, chain[0].ID)
	for _, f := range chain[1:] {
		if f.PassDown {
			add(f.PolicyIDs, f.ID)
		}
	}
	out.EffectivePolicies = policies
	return out, nil
}

func indexFolders(folders []Snapshot) (map[string]Snapshot, map[string][]string) {
	byID := make(map[string]Snapshot, len(folders))
	children := make(map[string][]string, len(folders))
	for _, f := range folders {
		byID[f.ID] = f
		children[f.ParentID] = append(children[f.ParentID], f.ID)
	}
	return byID, children
}

func subtreeExtra(children map[string][]string, id string) int {
	max := 0
	type item struct {
		id    string
		depth int
	}
	stack := []item{{id: id}}
	seen := map[string]bool{}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[n.id] {
			continue
		}
		seen[n.id] = true
		if n.depth > max {
			max = n.depth
		}
		for _, child := range children[n.id] {
			stack = append(stack, item{id: child, depth: n.depth + 1})
		}
	}
	return max
}

func cloneTags(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	return copyTags(in)
}

func copyTags(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
