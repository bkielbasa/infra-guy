package version

import (
	"context"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Version is the release tag when this binary was built from a tag,
// otherwise the short commit. The image build sets it with -ldflags -X.
var Version = "dev"

var (
	once    sync.Once
	current string
)

// Current returns the stamped build version, or the tag of a clean checkout
// of this commit, or the short commit.
func Current() string {
	once.Do(func() { current = resolve() })
	return current
}

func resolve() string {
	rev, modified := buildRevision()
	var tag string
	if head, ok := gitOutput("rev-parse", "HEAD"); ok && (rev == "" || head == rev) {
		if rev == "" {
			if dirty, ok := gitDirty(); ok {
				modified = dirty
			}
		}
		if exact, ok := gitOutput("describe", "--exact-match", "--tags", "HEAD"); ok {
			tag = exact
		}
	}
	return format(Version, tag, rev, modified)
}

func format(stamped, exactTag, revision string, dirty bool) string {
	if stamped != "" && stamped != "dev" {
		return stamped
	}
	if exactTag != "" && !dirty {
		return exactTag
	}
	if short := shortRevision(revision); short != "" {
		return short
	}
	if stamped != "" {
		return stamped
	}
	return "dev"
}

func shortRevision(revision string) string {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return ""
	}
	if len(revision) > 7 {
		return revision[:7]
	}
	return revision
}

func buildRevision() (revision string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return revision, modified
}

func gitDirty() (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "status", "--porcelain").Output()
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(string(out)) != "", true
}

func gitOutput(args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return "", false
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", false
	}
	return text, true
}
