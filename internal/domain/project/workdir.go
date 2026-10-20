package project

import (
	"errors"
	"path"
	"strings"
)

var ErrInvalidWorkingDirectory = errors.New("invalid working directory")

func ParseWorkingDirectory(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if strings.HasPrefix(s, "/") || strings.Contains(s, `\`) || path.IsAbs(s) {
		return "", ErrInvalidWorkingDirectory
	}
	clean := path.Clean(s)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrInvalidWorkingDirectory
	}
	return clean, nil
}
