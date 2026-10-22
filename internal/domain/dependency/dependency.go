package dependency

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"infra-guy/internal/domain/project"
)

var (
	ErrSelfDependency = errors.New("project cannot depend on itself")
	ErrCycleDetected  = errors.New("dependency would create a cycle")
	identPattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type ID string

type Mapping struct {
	ID         string
	OutputName string
	InputName  string
	CreatedAt  time.Time
}

func (m Mapping) EnvKey() string {
	if strings.HasPrefix(m.InputName, "TF_VAR_") {
		return m.InputName
	}
	return "TF_VAR_" + m.InputName
}

func NewMapping(id, outputName, inputName string, now time.Time) (Mapping, error) {
	if strings.TrimSpace(id) == "" {
		return Mapping{}, errors.New("mapping id is required")
	}
	outName := strings.TrimSpace(outputName)
	if !identPattern.MatchString(outName) {
		return Mapping{}, fmt.Errorf("invalid output name %q", outName)
	}
	inName := strings.TrimSpace(inputName)
	inName = strings.TrimPrefix(inName, "TF_VAR_")
	if inName == "" {
		inName = outName
	}
	if !identPattern.MatchString(inName) {
		return Mapping{}, fmt.Errorf("invalid input variable name %q", inName)
	}
	return Mapping{
		ID:         id,
		OutputName: outName,
		InputName:  inName,
		CreatedAt:  now.UTC(),
	}, nil
}

type Edge struct {
	ProjectID          project.ID
	DependsOnProjectID project.ID
}

type Snapshot struct {
	ID                 ID
	TenantID           string
	ProjectID          project.ID
	DependsOnProjectID project.ID
	TriggerAlways      bool
	Mappings           []Mapping
	CreatedAt          time.Time
}

type Dependency struct {
	s Snapshot
}

func New(id ID, tenantID string, projectID, dependsOnProjectID project.ID, triggerAlways bool, now time.Time) (*Dependency, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, errors.New("dependency id is required")
	}
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("tenant id is required")
	}
	if strings.TrimSpace(string(projectID)) == "" || strings.TrimSpace(string(dependsOnProjectID)) == "" {
		return nil, errors.New("project and upstream project are required")
	}
	if projectID == dependsOnProjectID {
		return nil, ErrSelfDependency
	}
	return &Dependency{
		s: Snapshot{
			ID:                 id,
			TenantID:           tenantID,
			ProjectID:          projectID,
			DependsOnProjectID: dependsOnProjectID,
			TriggerAlways:      triggerAlways,
			CreatedAt:          now.UTC(),
		},
	}, nil
}

func Restore(s Snapshot) *Dependency {
	cp := s
	cp.Mappings = append([]Mapping(nil), s.Mappings...)
	return &Dependency{s: cp}
}

func (d *Dependency) Snapshot() Snapshot {
	cp := d.s
	cp.Mappings = append([]Mapping(nil), d.s.Mappings...)
	return cp
}

func (d *Dependency) SetMappings(mappings []Mapping) {
	d.s.Mappings = append([]Mapping(nil), mappings...)
}

func (d *Dependency) SetTriggerAlways(v bool) {
	d.s.TriggerAlways = v
}

// WouldCreateCycle checks whether adding an edge (downstream depends on upstream)
// would introduce a cycle in the directed dependency graph.
func WouldCreateCycle(edges []Edge, downstream, upstream project.ID) bool {
	if downstream == upstream {
		return true
	}
	adj := make(map[project.ID][]project.ID)
	for _, e := range edges {
		adj[e.ProjectID] = append(adj[e.ProjectID], e.DependsOnProjectID)
	}
	visited := make(map[project.ID]bool)
	queue := []project.ID{upstream}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		if curr == downstream {
			return true
		}
		if visited[curr] {
			continue
		}
		visited[curr] = true
		for _, next := range adj[curr] {
			if !visited[next] {
				queue = append(queue, next)
			}
		}
	}
	return false
}

type Repository interface {
	Create(ctx context.Context, d *Dependency) error
	Get(ctx context.Context, id ID) (*Dependency, error)
	SetTriggerAlways(ctx context.Context, id ID, triggerAlways bool) error
	Delete(ctx context.Context, id ID) error
	UpsertMapping(ctx context.Context, depID ID, m Mapping) error
	DeleteMapping(ctx context.Context, depID ID, mappingID string) error
	ListByProject(ctx context.Context, projectID project.ID) ([]*Dependency, error)
	ListDownstream(ctx context.Context, upstreamProjectID project.ID) ([]*Dependency, error)
	ListByTenant(ctx context.Context, tenantID string) ([]*Dependency, error)
}
