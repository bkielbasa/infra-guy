package stateapp

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/state"
	"infra-guy/internal/infrastructure/crypto"
)

type Service struct {
	Projects project.Repository
	States   state.Repository
	Now      func() time.Time
}

type Outcome struct {
	Code int
	Body []byte
	MD5  string
}

func (s *Service) authorize(ctx context.Context, projectID, token string) (Outcome, error, bool) {
	p, err := s.Projects.Get(ctx, project.ID(projectID))
	if err != nil {
		return Outcome{}, err, false
	}
	if p == nil || token == "" {
		return Outcome{Code: 401}, nil, false
	}
	ps := p.Snapshot()
	if ps.StateTokenHash != "" {
		if crypto.VerifyTokenHash(token, ps.StateTokenHash) {
			return Outcome{}, nil, true
		}
		return Outcome{Code: 401}, nil, false
	}
	if ps.StateToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(ps.StateToken)) != 1 {
		return Outcome{Code: 401}, nil, false
	}
	return Outcome{}, nil, true
}

func (s *Service) loadState(ctx context.Context, projectID string) (*state.State, error) {
	st, err := s.States.Load(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return state.New(projectID), nil
	}
	return st, nil
}

func conflict(err error, lockJSON []byte) (Outcome, bool) {
	if errors.Is(err, state.ErrLocked) {
		return Outcome{Code: 409, Body: lockJSON}, true
	}
	if errors.Is(err, state.ErrLockMismatch) {
		return Outcome{Code: 409}, true
	}
	return Outcome{}, false
}

func (s *Service) Metadata(ctx context.Context, projectID string) (state.Metadata, error) {
	st, err := s.States.Load(ctx, projectID)
	if err != nil {
		return state.Metadata{}, err
	}
	if st == nil {
		return state.Metadata{}, nil
	}
	return st.Snapshot().Metadata(), nil
}

func (s *Service) Import(ctx context.Context, projectID string, data []byte) error {
	return s.ImportWithActor(ctx, projectID, data, "import")
}

func (s *Service) ImportWithActor(ctx context.Context, projectID string, data []byte, actor string) error {
	st, err := s.loadState(ctx, projectID)
	if err != nil {
		return err
	}
	if err := st.ImportWithActor(data, actor, s.Now()); err != nil {
		return err
	}
	return s.States.Save(ctx, st)
}

func (s *Service) ListVersions(ctx context.Context, projectID string) ([]state.VersionSummary, error) {
	if vr, ok := s.States.(state.VersionRepository); ok {
		return vr.ListVersions(ctx, projectID)
	}
	st, err := s.States.Load(ctx, projectID)
	if err != nil || st == nil {
		return nil, err
	}
	snap := st.Snapshot()
	if len(snap.Versions) > 0 {
		out := make([]state.VersionSummary, len(snap.Versions))
		for i, v := range snap.Versions {
			out[i] = v.VersionSummary
		}
		return out, nil
	}
	if len(snap.Data) > 0 && snap.Serial != nil {
		return []state.VersionSummary{
			{
				Serial:        *snap.Serial,
				Size:          len(snap.Data),
				RunID:         snap.LastRunID,
				Actor:         snap.LastActor,
				ResourceCount: state.CountResources(snap.Data),
				CreatedAt:     snap.UpdatedAt,
			},
		}, nil
	}
	return nil, nil
}

func (s *Service) GetVersion(ctx context.Context, projectID string, serial int64) (*state.Version, error) {
	if vr, ok := s.States.(state.VersionRepository); ok {
		v, err := vr.GetVersion(ctx, projectID, serial)
		if err != nil {
			return nil, err
		}
		if v != nil {
			return v, nil
		}
	}
	st, err := s.States.Load(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, state.ErrVersionNotFound
	}
	snap := st.Snapshot()
	for _, v := range snap.Versions {
		if v.Serial == serial {
			cp := v
			return &cp, nil
		}
	}
	if snap.Serial != nil && *snap.Serial == serial && len(snap.Data) > 0 {
		return &state.Version{
			VersionSummary: state.VersionSummary{
				Serial:        serial,
				Size:          len(snap.Data),
				RunID:         snap.LastRunID,
				Actor:         snap.LastActor,
				ResourceCount: state.CountResources(snap.Data),
				CreatedAt:     snap.UpdatedAt,
			},
			Data: snap.Data,
		}, nil
	}
	return nil, state.ErrVersionNotFound
}

func (s *Service) Diff(ctx context.Context, projectID string, fromSerial, toSerial int64) (state.StateDiff, error) {
	fromVer, err := s.GetVersion(ctx, projectID, fromSerial)
	if err != nil {
		return state.StateDiff{}, err
	}
	toVer, err := s.GetVersion(ctx, projectID, toSerial)
	if err != nil {
		return state.StateDiff{}, err
	}
	return state.DiffStates(fromSerial, fromVer.Data, toSerial, toVer.Data), nil
}

func (s *Service) Rollback(ctx context.Context, projectID string, targetSerial int64, actor string) (int64, error) {
	st, err := s.loadState(ctx, projectID)
	if err != nil {
		return 0, err
	}
	targetVer, err := s.GetVersion(ctx, projectID, targetSerial)
	if err != nil {
		return 0, err
	}
	if vr, ok := s.States.(state.VersionRepository); ok {
		if list, err := vr.ListVersions(ctx, projectID); err == nil && len(list) > 0 {
			snap := st.Snapshot()
			if snap.Serial == nil || list[0].Serial > *snap.Serial {
				maxS := list[0].Serial
				snap.Serial = &maxS
				st = state.Restore(snap)
			}
		}
	}
	actorLabel := actor
	if actorLabel == "" {
		actorLabel = fmt.Sprintf("rollback:#%d", targetSerial)
	}
	newSerial, err := st.Rollback(targetVer.Data, actorLabel, s.Now())
	if err != nil {
		return 0, err
	}
	if err := s.States.Save(ctx, st); err != nil {
		return 0, err
	}
	return newSerial, nil
}

func (s *Service) Outputs(ctx context.Context, projectID string) (map[string]state.OutputValue, error) {
	st, err := s.States.Load(ctx, projectID)
	if err != nil || st == nil {
		return nil, err
	}
	return st.Snapshot().Outputs(), nil
}

func (s *Service) ChangedOutputsSincePrevious(ctx context.Context, projectID string) (map[string]bool, error) {
	st, err := s.States.Load(ctx, projectID)
	if err != nil || st == nil {
		return nil, err
	}
	return st.Snapshot().ChangedOutputsSincePrevious(), nil
}

func (s *Service) ForceUnlock(ctx context.Context, projectID string) error {
	st, err := s.States.Load(ctx, projectID)
	if err != nil || st == nil {
		return err
	}
	st.ForceUnlock()
	return s.States.Save(ctx, st)
}

func (s *Service) Get(ctx context.Context, projectID, token string) (Outcome, error) {
	if out, err, ok := s.authorize(ctx, projectID, token); !ok {
		return out, err
	}
	st, err := s.loadState(ctx, projectID)
	if err != nil {
		return Outcome{}, err
	}
	if st.Empty() {
		return Outcome{Code: 404}, nil
	}
	body := st.Snapshot().Data
	sum := md5.Sum(body)
	return Outcome{
		Code: 200,
		Body: body,
		MD5:  base64.StdEncoding.EncodeToString(sum[:]),
	}, nil
}

func (s *Service) Put(ctx context.Context, projectID, token, lockID string, body []byte, runID string) (Outcome, error) {
	if out, err, ok := s.authorize(ctx, projectID, token); !ok {
		return out, err
	}
	st, err := s.loadState(ctx, projectID)
	if err != nil {
		return Outcome{}, err
	}
	if err := st.Put(body, lockID, runID, s.Now()); err != nil {
		if out, handled := conflict(err, st.Snapshot().LockInfo); handled {
			return out, nil
		}
		return Outcome{}, err
	}
	if err := s.States.Save(ctx, st); err != nil {
		return Outcome{}, err
	}
	return Outcome{Code: 200}, nil
}

func (s *Service) Lock(ctx context.Context, projectID, token string, info []byte) (Outcome, error) {
	if out, err, ok := s.authorize(ctx, projectID, token); !ok {
		return out, err
	}
	st, err := s.loadState(ctx, projectID)
	if err != nil {
		return Outcome{}, err
	}
	existing, err := st.Lock(info)
	if err != nil {
		if out, handled := conflict(err, existing); handled {
			return out, nil
		}
		return Outcome{}, err
	}
	if err := s.States.Save(ctx, st); err != nil {
		return Outcome{}, err
	}
	return Outcome{Code: 200}, nil
}

func (s *Service) Unlock(ctx context.Context, projectID, token, lockID string) (Outcome, error) {
	if out, err, ok := s.authorize(ctx, projectID, token); !ok {
		return out, err
	}
	st, err := s.loadState(ctx, projectID)
	if err != nil {
		return Outcome{}, err
	}
	if err := st.Unlock(lockID); err != nil {
		if out, handled := conflict(err, st.Snapshot().LockInfo); handled {
			return out, nil
		}
		return Outcome{}, err
	}
	if err := s.States.Save(ctx, st); err != nil {
		return Outcome{}, err
	}
	return Outcome{Code: 200}, nil
}
