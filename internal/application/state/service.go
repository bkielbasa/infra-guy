package stateapp

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"time"

	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/state"
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
	if p == nil || subtle.ConstantTimeCompare([]byte(token), []byte(p.Snapshot().StateToken)) != 1 {
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
