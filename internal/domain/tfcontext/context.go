package tfcontext

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"infra-guy/internal/domain/project"
)

type ID string

type EntryKind string

const (
	KindPlainEnv   EntryKind = "plain_env"
	KindSecretEnv  EntryKind = "secret_env"
	KindSecretFile EntryKind = "secret_file"
)

var (
	ErrInvalidEnvKey   = errors.New("invalid or reserved environment variable name")
	ErrInvalidFilePath = errors.New("invalid relative file path in $HOME")
	ErrInvalidKind     = errors.New("invalid context entry kind")
)

var reservedEnvKeys = map[string]bool{
	"PATH":                        true,
	"HOME":                        true,
	"USER":                        true,
	"TF_IN_AUTOMATION":            true,
	"TF_INPUT":                    true,
	"DATABASE_URL":                true,
	"POSTGRES_PASSWORD":           true,
	"BASIC_AUTH_USER":             true,
	"BASIC_AUTH_PASSWORD":         true,
	"GITHUB_APP_ID":               true,
	"GITHUB_APP_PRIVATE_KEY":      true,
	"GITHUB_APP_PRIVATE_KEY_FILE": true,
	"GITHUB_APP_WEBHOOK_SECRET":   true,
	"GITHUB_APP_CLIENT_ID":        true,
	"GITHUB_APP_CLIENT_SECRET":    true,
	"CONTEXT_ENCRYPTION_KEY":      true,
	"TF_LOG":                      true,
	"GIT_TRACE":                   true,
}

type Entry struct {
	ID         string
	Kind       EntryKind
	Key        string
	Value      string
	Ciphertext []byte
	Secret     bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type Snapshot struct {
	ID        ID
	TenantID  string
	Name      string
	Entries   []Entry
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Context struct{ s Snapshot }

func New(id ID, tenantID, name string, now time.Time) (*Context, error) {
	name = strings.TrimSpace(name)
	if id == "" || strings.TrimSpace(tenantID) == "" || name == "" {
		return nil, fmt.Errorf("context requires id, tenant_id, and name")
	}
	return &Context{s: Snapshot{
		ID: id, TenantID: tenantID, Name: name, CreatedAt: now, UpdatedAt: now,
	}}, nil
}

func Restore(s Snapshot) *Context {
	cp := s
	cp.Entries = append([]Entry(nil), s.Entries...)
	return &Context{s: cp}
}

func (c *Context) Snapshot() Snapshot {
	cp := c.s
	cp.Entries = append([]Entry(nil), c.s.Entries...)
	return cp
}

func (c *Context) SetEntries(entries []Entry) {
	c.s.Entries = append([]Entry(nil), entries...)
}

func ValidateEnvKey(raw string) (string, error) {
	k := strings.TrimSpace(raw)
	if k == "" || reservedEnvKeys[k] {
		return "", ErrInvalidEnvKey
	}
	for i := 0; i < len(k); i++ {
		ch := k[i]
		isAlpha := (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch == '_'
		isDigit := ch >= '0' && ch <= '9'
		if i == 0 && !isAlpha {
			return "", ErrInvalidEnvKey
		}
		if !isAlpha && !isDigit {
			return "", ErrInvalidEnvKey
		}
	}
	return k, nil
}

func ValidateFilePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	p = strings.TrimPrefix(p, "~/")
	if p == "" || p == "~" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return "", ErrInvalidFilePath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", ErrInvalidFilePath
		}
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", ErrInvalidFilePath
	}
	return clean, nil
}

func NewEntry(id string, kind EntryKind, key, value string, now time.Time) (Entry, error) {
	if id == "" {
		return Entry{}, fmt.Errorf("entry id is required")
	}
	var normKey string
	var err error
	var secret bool
	switch kind {
	case KindPlainEnv:
		normKey, err = ValidateEnvKey(key)
	case KindSecretEnv:
		normKey, err = ValidateEnvKey(key)
		secret = true
	case KindSecretFile:
		normKey, err = ValidateFilePath(key)
		secret = true
	default:
		return Entry{}, ErrInvalidKind
	}
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		ID: id, Kind: kind, Key: normKey, Value: value, Secret: secret,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func Resolve(contexts []*Context) (map[string]string, map[string][]byte) {
	env := make(map[string]string)
	files := make(map[string][]byte)
	for _, c := range contexts {
		if c == nil {
			continue
		}
		for _, e := range c.s.Entries {
			switch e.Kind {
			case KindPlainEnv, KindSecretEnv:
				env[e.Key] = e.Value
			case KindSecretFile:
				files[e.Key] = []byte(e.Value)
			}
		}
	}
	return env, files
}

type Repository interface {
	Create(ctx context.Context, c *Context) error
	Get(ctx context.Context, id ID) (*Context, error)
	ListByTenant(ctx context.Context, tenantID string) ([]*Context, error)
	Delete(ctx context.Context, id ID) error
	UpsertEntry(ctx context.Context, contextID ID, entry Entry) error
	DeleteEntry(ctx context.Context, contextID ID, entryID string) error
	SetProjectContexts(ctx context.Context, projectID project.ID, contextIDs []ID) error
	ListByProject(ctx context.Context, projectID project.ID) ([]*Context, error)
}
