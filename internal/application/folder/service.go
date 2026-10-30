package folderapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"infra-guy/internal/domain/folder"
	"infra-guy/internal/domain/project"
)

var (
	errNameTaken  = errors.New("a folder with that name already exists here")
	errRootDelete = errors.New("the root folder cannot be deleted")
	errRootTags   = errors.New("the root folder must set its own tags")
)

// Repository is the folder store. EnsureRoot inserts the tenant root when none exists.
type Repository interface {
	ListByTenant(ctx context.Context, tenantID string) ([]folder.Snapshot, error)
	Save(ctx context.Context, s folder.Snapshot) error
	EnsureRoot(ctx context.Context, tenantID, id string, now time.Time) (folder.Snapshot, error)
	DeleteAndReparent(ctx context.Context, tenantID, folderID, parentID string, now time.Time) error
}

type Service struct {
	Folders Repository
	Now     func() time.Time
	NewID   func() string
}

type CreateInput struct {
	TenantID  string
	ParentID  string
	Name      string
	PassDown  bool
	PolicyIDs []string
	TagsSet   bool
	Tags      map[string]string
}

// SettingsInput updates a folder. When UpdateTags is false, pass-down, policy ids, and tags stay as stored.
type SettingsInput struct {
	TenantID   string
	ID         string
	Name       string
	PassDown   bool
	PolicyIDs  []string
	TagsSet    bool
	Tags       map[string]string
	UpdateTags bool
}

func (s *Service) EnsureRoot(ctx context.Context, tenantID string) (folder.Snapshot, error) {
	folders, err := s.list(ctx, tenantID)
	if err != nil {
		return folder.Snapshot{}, err
	}
	for _, f := range folders {
		if f.ParentID == "" {
			return f, nil
		}
	}
	id, err := s.newID()
	if err != nil {
		return folder.Snapshot{}, err
	}
	return s.Folders.EnsureRoot(ctx, tenantID, id, s.now())
}

// List returns every folder in the tenant.
func (s *Service) List(ctx context.Context, tenantID string) ([]folder.Snapshot, error) {
	return s.list(ctx, tenantID)
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (folder.Snapshot, error) {
	folders, err := s.list(ctx, tenantID)
	if err != nil {
		return folder.Snapshot{}, err
	}
	current, ok := findFolder(folders, id)
	if !ok {
		return folder.Snapshot{}, folderNotFound(id)
	}
	return current, nil
}

func (s *Service) Effective(ctx context.Context, p project.Snapshot) (project.Snapshot, error) {
	folders, err := s.list(ctx, p.TenantID)
	if err != nil {
		return project.Snapshot{}, err
	}
	return folder.Resolve(p, folders)
}

// EffectiveAll resolves every project with one folder listing.
func (s *Service) EffectiveAll(ctx context.Context, tenantID string, projects []project.Snapshot) ([]project.Snapshot, error) {
	out := make([]project.Snapshot, 0, len(projects))
	if len(projects) == 0 {
		return out, nil
	}
	folders, err := s.list(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		resolved, err := folder.Resolve(p, folders)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

func (s *Service) Create(ctx context.Context, in CreateInput) (folder.Snapshot, error) {
	name, err := folder.ParseName(in.Name)
	if err != nil {
		return folder.Snapshot{}, err
	}
	folders, err := s.list(ctx, in.TenantID)
	if err != nil {
		return folder.Snapshot{}, err
	}
	parent, ok := findFolder(folders, in.ParentID)
	if !ok {
		return folder.Snapshot{}, folderNotFound(in.ParentID)
	}
	depth, err := folderDepth(folders, parent.ID)
	if err != nil {
		return folder.Snapshot{}, err
	}
	if depth+1 > 8 {
		return folder.Snapshot{}, errors.New("folder depth cannot exceed 8")
	}
	if siblingNameTaken(folders, parent.ID, "", name) {
		return folder.Snapshot{}, errNameTaken
	}
	id, err := s.newID()
	if err != nil {
		return folder.Snapshot{}, err
	}
	now := s.now()
	snap := folder.Snapshot{
		ID:        id,
		TenantID:  in.TenantID,
		ParentID:  parent.ID,
		Name:      name,
		PassDown:  in.PassDown,
		TagsSet:   in.TagsSet,
		Tags:      storedTags(in.TagsSet, in.Tags),
		PolicyIDs: copyIDs(in.PolicyIDs),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.save(ctx, snap); err != nil {
		return folder.Snapshot{}, err
	}
	return snap, nil
}

func (s *Service) UpdateSettings(ctx context.Context, in SettingsInput) (folder.Snapshot, error) {
	name, err := folder.ParseName(in.Name)
	if err != nil {
		return folder.Snapshot{}, err
	}
	folders, err := s.list(ctx, in.TenantID)
	if err != nil {
		return folder.Snapshot{}, err
	}
	current, ok := findFolder(folders, in.ID)
	if !ok {
		return folder.Snapshot{}, folderNotFound(in.ID)
	}
	if current.ParentID == "" && in.UpdateTags && !in.TagsSet {
		return folder.Snapshot{}, errRootTags
	}
	if siblingNameTaken(folders, current.ParentID, current.ID, name) {
		return folder.Snapshot{}, errNameTaken
	}
	current.Name = name
	current.UpdatedAt = s.now()
	if in.UpdateTags {
		current.PassDown = in.PassDown
		current.PolicyIDs = copyIDs(in.PolicyIDs)
		current.TagsSet = in.TagsSet
		current.Tags = storedTags(in.TagsSet, in.Tags)
	}
	if err := s.save(ctx, current); err != nil {
		return folder.Snapshot{}, err
	}
	return current, nil
}

func (s *Service) Move(ctx context.Context, tenantID, id, newParentID string) error {
	folders, err := s.list(ctx, tenantID)
	if err != nil {
		return err
	}
	current, ok := findFolder(folders, id)
	if !ok {
		return folderNotFound(id)
	}
	newParentID = strings.TrimSpace(newParentID)
	if err := folder.ValidateMove(folders, current.ID, newParentID); err != nil {
		return err
	}
	if siblingNameTaken(folders, newParentID, current.ID, current.Name) {
		return errNameTaken
	}
	parent, ok := findFolder(folders, newParentID)
	if !ok {
		return folderNotFound(newParentID)
	}
	current.ParentID = parent.ID
	current.UpdatedAt = s.now()
	return s.save(ctx, current)
}

func (s *Service) Delete(ctx context.Context, tenantID, id string) (string, error) {
	folders, err := s.list(ctx, tenantID)
	if err != nil {
		return "", err
	}
	current, ok := findFolder(folders, id)
	if !ok {
		return "", folderNotFound(id)
	}
	if current.ParentID == "" {
		return "", errRootDelete
	}
	if reparentNameClash(folders, current.ID, current.ParentID) {
		return "", errNameTaken
	}
	if err := s.Folders.DeleteAndReparent(ctx, tenantID, current.ID, current.ParentID, s.now()); err != nil {
		return "", err
	}
	return current.ParentID, nil
}

func (s *Service) list(ctx context.Context, tenantID string) ([]folder.Snapshot, error) {
	if s.Folders == nil {
		return nil, errors.New("folder repository is unavailable")
	}
	return s.Folders.ListByTenant(ctx, tenantID)
}

func (s *Service) save(ctx context.Context, snap folder.Snapshot) error {
	if err := s.Folders.Save(ctx, snap); err != nil {
		if uniqueViolation(err) {
			return errNameTaken
		}
		return err
	}
	return nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) newID() (string, error) {
	if s.NewID != nil {
		id := strings.TrimSpace(s.NewID())
		if id == "" {
			return "", errors.New("missing id generator")
		}
		return id, nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func folderNotFound(id string) error {
	return fmt.Errorf("folder %s not found", strings.TrimSpace(id))
}

func findFolder(folders []folder.Snapshot, id string) (folder.Snapshot, bool) {
	id = strings.TrimSpace(id)
	for _, f := range folders {
		if f.ID == id {
			return f, true
		}
	}
	return folder.Snapshot{}, false
}

func folderDepth(folders []folder.Snapshot, id string) (int, error) {
	byID := make(map[string]folder.Snapshot, len(folders))
	for _, f := range folders {
		byID[f.ID] = f
	}
	depth := 0
	seen := map[string]bool{}
	for cur := id; cur != ""; {
		if seen[cur] {
			return 0, folderNotFound(cur)
		}
		seen[cur] = true
		f, ok := byID[cur]
		if !ok {
			return 0, folderNotFound(cur)
		}
		depth++
		cur = f.ParentID
	}
	return depth, nil
}

func siblingNameTaken(folders []folder.Snapshot, parentID, selfID, name string) bool {
	for _, f := range folders {
		if f.ParentID == parentID && f.ID != selfID && strings.EqualFold(f.Name, name) {
			return true
		}
	}
	return false
}

func reparentNameClash(folders []folder.Snapshot, folderID, newParentID string) bool {
	for _, child := range folders {
		if child.ParentID != folderID {
			continue
		}
		if siblingNameTaken(folders, newParentID, folderID, child.Name) {
			return true
		}
	}
	return false
}

func storedTags(set bool, tags map[string]string) map[string]string {
	if !set {
		return map[string]string{}
	}
	return copyTags(tags)
}

func copyTags(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyIDs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

type sqlStateError interface {
	SQLState() string
}

func uniqueViolation(err error) bool {
	var se sqlStateError
	return errors.As(err, &se) && se.SQLState() == "23505"
}
