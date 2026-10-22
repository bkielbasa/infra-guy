package dependencyapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	runapp "infra-guy/internal/application/run"
	"infra-guy/internal/domain/dependency"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/state"
)

var ErrNotFound = errors.New("dependency not found")

type RunOpener interface {
	Open(ctx context.Context, req runapp.OpenRequest) (*run.Run, error)
}

type TokenSource interface {
	InstallationToken(ctx context.Context, tenantID string) (string, error)
}

type Brancher interface {
	BranchTip(ctx context.Context, cloneURL, token, branch string) (string, error)
}

type CommitLookup interface {
	Lookup(ctx context.Context, repo project.SourceRepo, token, sha string) (message, author string, err error)
}

type Service struct {
	Dependencies dependency.Repository
	Projects     project.Repository
	States       state.Repository
	Runs         RunOpener
	Tokens       TokenSource
	Checkout     Brancher
	Lookup       CommitLookup
	Now          func() time.Time
	NewID        func() string
}

func (s *Service) Get(ctx context.Context, tenantID string, id dependency.ID) (*dependency.Dependency, error) {
	d, err := s.Dependencies.Get(ctx, id)
	if err != nil || d == nil {
		return nil, err
	}
	if d.Snapshot().TenantID != tenantID {
		return nil, nil
	}
	return d, nil
}

func (s *Service) Create(ctx context.Context, tenantID string, projectID, dependsOnProjectID project.ID, triggerAlways bool) (*dependency.Dependency, error) {
	p, err := s.Projects.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if p == nil || p.Snapshot().TenantID != tenantID {
		return nil, ErrNotFound
	}
	up, err := s.Projects.Get(ctx, dependsOnProjectID)
	if err != nil {
		return nil, err
	}
	if up == nil || up.Snapshot().TenantID != tenantID {
		return nil, fmt.Errorf("upstream project not found")
	}

	existing, err := s.Dependencies.ListByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	edges := make([]dependency.Edge, 0, len(existing))
	for _, item := range existing {
		is := item.Snapshot()
		if is.ProjectID == projectID && is.DependsOnProjectID == dependsOnProjectID {
			return nil, fmt.Errorf("dependency on %s already exists", up.Snapshot().Name)
		}
		edges = append(edges, dependency.Edge{
			ProjectID:          is.ProjectID,
			DependsOnProjectID: is.DependsOnProjectID,
		})
	}
	if dependency.WouldCreateCycle(edges, projectID, dependsOnProjectID) {
		return nil, dependency.ErrCycleDetected
	}

	d, err := dependency.New(dependency.ID(s.NewID()), tenantID, projectID, dependsOnProjectID, triggerAlways, s.Now())
	if err != nil {
		return nil, err
	}
	if err := s.Dependencies.Create(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Service) SetTriggerAlways(ctx context.Context, tenantID string, depID dependency.ID, triggerAlways bool) error {
	d, err := s.Get(ctx, tenantID, depID)
	if err != nil {
		return err
	}
	if d == nil {
		return ErrNotFound
	}
	return s.Dependencies.SetTriggerAlways(ctx, depID, triggerAlways)
}

func (s *Service) Delete(ctx context.Context, tenantID string, depID dependency.ID) error {
	d, err := s.Get(ctx, tenantID, depID)
	if err != nil {
		return err
	}
	if d == nil {
		return ErrNotFound
	}
	return s.Dependencies.Delete(ctx, depID)
}

func (s *Service) AddMapping(ctx context.Context, tenantID string, depID dependency.ID, outputName, inputName string) error {
	d, err := s.Get(ctx, tenantID, depID)
	if err != nil {
		return err
	}
	if d == nil {
		return ErrNotFound
	}
	m, err := dependency.NewMapping(s.NewID(), outputName, inputName, s.Now())
	if err != nil {
		return err
	}
	return s.Dependencies.UpsertMapping(ctx, depID, m)
}

func (s *Service) RemoveMapping(ctx context.Context, tenantID string, depID dependency.ID, mappingID string) error {
	d, err := s.Get(ctx, tenantID, depID)
	if err != nil {
		return err
	}
	if d == nil {
		return ErrNotFound
	}
	return s.Dependencies.DeleteMapping(ctx, depID, mappingID)
}

func (s *Service) ListByProject(ctx context.Context, projectID project.ID) ([]*dependency.Dependency, error) {
	return s.Dependencies.ListByProject(ctx, projectID)
}

func (s *Service) ListDownstream(ctx context.Context, upstreamProjectID project.ID) ([]*dependency.Dependency, error) {
	return s.Dependencies.ListDownstream(ctx, upstreamProjectID)
}

func (s *Service) ResolveForProject(ctx context.Context, projectID project.ID) (map[string]string, []string, error) {
	deps, err := s.Dependencies.ListByProject(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	env := make(map[string]string)
	var messages []string
	for _, dep := range deps {
		ds := dep.Snapshot()
		if len(ds.Mappings) == 0 {
			continue
		}
		upProj, err := s.Projects.Get(ctx, ds.DependsOnProjectID)
		if err != nil {
			return nil, nil, err
		}
		upName := string(ds.DependsOnProjectID)
		if upProj != nil {
			upName = upProj.Snapshot().Name
		}
		var outputs map[string]state.OutputValue
		if s.States != nil {
			st, err := s.States.Load(ctx, string(ds.DependsOnProjectID))
			if err != nil {
				return nil, nil, err
			}
			if st != nil {
				outputs = st.Snapshot().Outputs()
			}
		}
		for _, m := range ds.Mappings {
			outVal, ok := outputs[m.OutputName]
			if !ok {
				messages = append(messages, fmt.Sprintf("Warning: upstream project %q has no output %q (mapped to %s)", upName, m.OutputName, m.EnvKey()))
				continue
			}
			env[m.EnvKey()] = outVal.EnvString
			messages = append(messages, fmt.Sprintf("Injected %s from upstream project %q output %q", m.EnvKey(), upName, m.OutputName))
		}
	}
	return env, messages, nil
}

func (s *Service) ShouldTriggerDownstream(dep dependency.Snapshot, changedOutputs map[string]bool) bool {
	if dep.TriggerAlways {
		return true
	}
	for _, m := range dep.Mappings {
		if changedOutputs[m.OutputName] {
			return true
		}
	}
	return false
}

func (s *Service) TriggerDownstreamRuns(ctx context.Context, upstreamProj *project.Project, finishedRun *run.Run) {
	if s == nil || upstreamProj == nil || finishedRun == nil || s.Runs == nil || s.Tokens == nil || s.Checkout == nil {
		return
	}
	rs := finishedRun.Snapshot()
	if rs.PlanOnly || rs.Status != run.StatusSucceeded {
		return
	}
	ps := upstreamProj.Snapshot()

	var changed map[string]bool
	if s.States != nil {
		if st, err := s.States.Load(ctx, string(ps.ID)); err == nil && st != nil {
			changed = st.Snapshot().ChangedOutputsSincePrevious()
		}
	}

	deps, err := s.Dependencies.ListDownstream(ctx, ps.ID)
	if err != nil {
		log.Printf("pipeline list downstream for %s: %v", ps.ID, err)
		return
	}
	for _, dep := range deps {
		ds := dep.Snapshot()
		if !s.ShouldTriggerDownstream(ds, changed) {
			continue
		}
		dp, err := s.Projects.Get(ctx, ds.ProjectID)
		if err != nil || dp == nil {
			continue
		}
		dps := dp.Snapshot()
		if dps.Locked {
			continue
		}
		token, err := s.Tokens.InstallationToken(ctx, dps.TenantID)
		if err != nil {
			log.Printf("pipeline token for downstream %s: %v", dps.ID, err)
			continue
		}
		sha, err := s.Checkout.BranchTip(ctx, dps.Repo.CloneURL(), token, dps.DefaultBranch)
		if err != nil {
			log.Printf("pipeline branch tip for downstream %s: %v", dps.ID, err)
			continue
		}
		var msg, author string
		if s.Lookup != nil {
			msg, author, _ = s.Lookup.Lookup(ctx, dps.Repo, token, sha)
		}
		if _, err := s.Runs.Open(ctx, runapp.OpenRequest{
			ProjectID:   dps.ID,
			Trigger:     run.TriggerPipeline,
			SHA:         sha,
			Branch:      dps.DefaultBranch,
			Message:     msg,
			Author:      author,
			TriggeredBy: "pipeline:" + ps.Name,
			PlanOnly:    false,
		}); err != nil && !errors.Is(err, runapp.ErrDuplicate) {
			log.Printf("pipeline open run for downstream %s: %v", dps.ID, err)
		}
	}
}
