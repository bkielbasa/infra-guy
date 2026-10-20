package project

import (
	"errors"
	"strings"
)

var ErrInvalidRepo = errors.New("invalid github repository")

type SourceRepo struct {
	Owner string
	Name  string
}

func ParseSourceRepo(raw string) (SourceRepo, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimSuffix(s, ".git")
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return SourceRepo{}, ErrInvalidRepo
	}
	return SourceRepo{Owner: owner, Name: name}, nil
}

func (r SourceRepo) String() string { return r.Owner + "/" + r.Name }

func (r SourceRepo) CloneURL() string {
	return "https://github.com/" + r.Owner + "/" + r.Name + ".git"
}
