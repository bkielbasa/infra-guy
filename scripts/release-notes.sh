#!/usr/bin/env bash
# Generates structured Markdown release notes grouped by change type (feat, fix,
# perf, docs, refactor, security, chore) and component (<type>(<component>): <description>),
# combining both Git commits and referenced/closed GitHub tickets.
#
# Usage:
#   ./scripts/release-notes.sh [TARGET_TAG_OR_REF] [PREV_TAG]
set -euo pipefail

REPO="${REPO:-bkielbasa/infra-guy}"
TARGET_REF="${1:-HEAD}"
PREV_TAG="${2:-}"

cd "$(git rev-parse --show-toplevel)"

if [[ -z "$PREV_TAG" ]]; then
  if [[ "$TARGET_REF" != "HEAD" ]] && git rev-parse "$TARGET_REF" >/dev/null 2>&1; then
    PREV_TAG="$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | awk -v cur="$TARGET_REF" '
      $0 == cur { found=1; next }
      found { print; exit }
    ')"
  fi
  if [[ -z "$PREV_TAG" ]]; then
    PREV_TAG="$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | grep -Fxv "$TARGET_REF" | head -n 1 || true)"
  fi
fi

GIT_TARGET="HEAD"
if git rev-parse "$TARGET_REF" >/dev/null 2>&1; then
  GIT_TARGET="$TARGET_REF"
fi

if [[ -n "$PREV_TAG" ]]; then
  RANGE="${PREV_TAG}..${GIT_TARGET}"
else
  RANGE="${GIT_TARGET}"
fi

TMP_COMMITS="$(mktemp)"
TMP_ISSUES="$(mktemp)"
trap 'rm -f "$TMP_COMMITS" "$TMP_ISSUES"' EXIT

# Export commits as JSON-friendly records with changed files
python3 - "$RANGE" "$TMP_COMMITS" <<'PYEOF'
import json
import subprocess
import sys

git_range = sys.argv[1]
out_path = sys.argv[2]

raw = subprocess.check_output(
    ["git", "log", git_range, "--no-merges", "--pretty=format:%H%x1f%h%x1f%cI%x1f%s%x1f%b%x1e"],
    text=True,
)

commits = []
for rec in raw.split("\x1e"):
    rec = rec.strip()
    if not rec:
        continue
    parts = rec.split("\x1f")
    if len(parts) < 4:
        continue
    full_sha, short_sha, committed_at, subject = parts[0], parts[1], parts[2], parts[3]
    body = parts[4] if len(parts) > 4 else ""
    files_raw = subprocess.check_output(
        ["git", "diff-tree", "--no-commit-id", "--name-only", "-r", full_sha],
        text=True,
    )
    files = [f.strip() for f in files_raw.splitlines() if f.strip()]
    commits.append({
        "sha": full_sha,
        "short_sha": short_sha,
        "committed_at": committed_at,
        "subject": subject.strip(),
        "body": body.strip(),
        "files": files,
    })

with open(out_path, "w", encoding="utf-8") as f:
    json.dump(commits, f)
PYEOF

# Fetch GitHub issues (if gh is available and authenticated)
if command -v gh >/dev/null 2>&1 && gh issue list --repo "$REPO" --state all --limit 200 --json number,title,state,closedAt,url >"$TMP_ISSUES" 2>/dev/null; then
  :
else
  echo "[]" >"$TMP_ISSUES"
fi

PREV_TAG_TIME=""
if [[ -n "$PREV_TAG" ]]; then
  PREV_TAG_TIME="$(git log -1 --format=%cI "$PREV_TAG" 2>/dev/null || true)"
fi
TARGET_TAG_TIME="$(git log -1 --format=%cI "$GIT_TARGET" 2>/dev/null || true)"

python3 - "$REPO" "$TARGET_REF" "$PREV_TAG" "$PREV_TAG_TIME" "$TARGET_TAG_TIME" "$TMP_COMMITS" "$TMP_ISSUES" <<'PYEOF'
import json
import re
import sys
from collections import defaultdict
from datetime import datetime

repo = sys.argv[1]
target_ref = sys.argv[2]
prev_tag = sys.argv[3]
prev_tag_time = sys.argv[4]
target_tag_time = sys.argv[5]
commits_path = sys.argv[6]
issues_path = sys.argv[7]

with open(commits_path, encoding="utf-8") as f:
    commits = json.load(f)

with open(issues_path, encoding="utf-8") as f:
    issues_list = json.load(f)

issues_by_num = {int(i["number"]): i for i in issues_list if "number" in i}

TYPE_ORDER = [
    ("feat", "🚀 Features"),
    ("fix", "🐛 Bug Fixes"),
    ("security", "🔒 Security"),
    ("perf", "⚡ Performance"),
    ("refactor", "♻️ Refactoring"),
    ("docs", "📚 Documentation"),
    ("test", "🧪 Tests"),
    ("chore", "🔧 Chores & CI"),
]

TYPE_ALIASES = {
    "feat": "feat",
    "feature": "feat",
    "fix": "fix",
    "bug": "fix",
    "bugfix": "fix",
    "sec": "security",
    "security": "security",
    "perf": "perf",
    "refactor": "refactor",
    "doc": "docs",
    "docs": "docs",
    "test": "test",
    "tests": "test",
    "ci": "chore",
    "build": "chore",
    "chore": "chore",
}

CONV_RE = re.compile(r"^([a-zA-Z]+)(?:\(([^)]+)\))?!?:\s*(.+)$")
ISSUE_REF_RE = re.compile(r"#(\d+)")
RELEASE_CHORE_RE = re.compile(r"^chore(?:\([^)]+\))?:\s*(?:release|bump)\s+v?\d+\.\d+\.\d+", re.IGNORECASE)


def parse_iso(ts: str):
    if not ts:
        return None
    try:
        return datetime.fromisoformat(ts.replace("Z", "+00:00"))
    except ValueError:
        return None


def infer_component_from_files(files):
    if not files:
        return "core"
    for f in files:
        if "internal/interfaces/http/templates/" in f or "internal/interfaces/http/static/" in f:
            return "ui"
        if "webhook" in f:
            return "webhook"
        if "/audit" in f or "audit_log" in f:
            return "audit"
        if "/policy" in f or "policies" in f:
            return "policy"
        if "/tfcontext" in f or "contexts" in f:
            return "context"
        if "/state" in f:
            return "state"
        if "/worker" in f or "/terraform" in f:
            return "worker"
        if "/auth" in f or "idp" in f:
            return "auth"
        if "/crypto" in f:
            return "security"
        if "/postgres" in f:
            return "db"
        if f.startswith("docs/") or f == "README.md":
            return "docs"
        if f.startswith(".github/") or f.startswith("scripts/") or f == "Dockerfile":
            return "ci"
    return "core"


def infer_type_from_text(text, files):
    low = text.lower()
    if any(f.startswith("docs/") or f == "README.md" for f in files) and len(files) == 1:
        return "docs"
    if low.startswith(("fix", "bug", "hide ", "resolve", "prevent", "repair")):
        return "fix"
    if low.startswith(("perf", "optimize", "eliminate n+1")):
        return "perf"
    if low.startswith(("refactor", "clean up", "extract", "decouple")):
        return "refactor"
    if low.startswith(("doc", "readme")):
        return "docs"
    if low.startswith(("chore", "ci", "build", "bump")):
        return "chore"
    return "feat"


def parse_structured_title(title, fallback_files=None):
    m = CONV_RE.match(title.strip())
    if m:
        raw_type, raw_comp, desc = m.group(1).lower(), m.group(2), m.group(3).strip()
        norm_type = TYPE_ALIASES.get(raw_type, "feat")
        if raw_comp:
            comp = raw_comp.strip().lower()
        else:
            comp = infer_component_from_files(fallback_files or [])
        return norm_type, comp, desc
    norm_type = infer_type_from_text(title, fallback_files or [])
    comp = infer_component_from_files(fallback_files or [])
    return norm_type, comp, title.strip()


# Group structure: grouped[change_type][component] -> list of markdown bullet lines
grouped = defaultdict(lambda: defaultdict(list))
seen_issues = set()

# 1. Process commits in chronological/reverse-chronological order
for c in commits:
    subj = c["subject"]
    if RELEASE_CHORE_RE.match(subj):
        continue

    full_text = f"{subj}\n{c['body']}"
    ref_nums = [int(n) for n in ISSUE_REF_RE.findall(full_text)]

    # If the commit references known GitHub issues, create an entry per referenced issue
    matched_known_issue = False
    for num in ref_nums:
        if num in issues_by_num:
            matched_known_issue = True
            if num in seen_issues:
                continue
            seen_issues.add(num)
            issue = issues_by_num[num]
            c_type, comp, desc = parse_structured_title(issue["title"], c["files"])
            desc_clean = desc[0].upper() + desc[1:] if desc else issue["title"]
            issue_url = issue.get("url") or f"https://github.com/{repo}/issues/{num}"
            commit_url = f"https://github.com/{repo}/commit/{c['sha']}"
            line = f"- {desc_clean} ([#{num}]({issue_url})) ([`{c['short_sha']}`]({commit_url}))"
            grouped[c_type][comp].append(line)

    if not matched_known_issue:
        c_type, comp, desc = parse_structured_title(subj, c["files"])
        # Remove trailing (#...) if already in text
        desc_clean = desc[0].upper() + desc[1:] if desc else subj
        commit_url = f"https://github.com/{repo}/commit/{c['sha']}"
        line = f"- {desc_clean} ([`{c['short_sha']}`]({commit_url}))"
        grouped[c_type][comp].append(line)

# 2. Also include any issues closed in the release window that were not explicitly cited in a commit
t_start = parse_iso(prev_tag_time)
t_end = parse_iso(target_tag_time)
if t_start:
    for num, issue in sorted(issues_by_num.items()):
        if num in seen_issues or issue.get("state") != "CLOSED":
            continue
        t_closed = parse_iso(issue.get("closedAt") or "")
        if not t_closed:
            continue
        if t_closed > t_start and (not t_end or t_closed <= t_end):
            seen_issues.add(num)
            c_type, comp, desc = parse_structured_title(issue["title"], [])
            desc_clean = desc[0].upper() + desc[1:] if desc else issue["title"]
            issue_url = issue.get("url") or f"https://github.com/{repo}/issues/{num}"
            line = f"- {desc_clean} ([#{num}]({issue_url}))"
            grouped[c_type][comp].append(line)

lines = []
header_ver = target_ref if target_ref != "HEAD" else "Unreleased"
lines.append(f"## What's Changed in {header_ver}")
lines.append("")

any_entries = False
for type_key, type_heading in TYPE_ORDER:
    comp_map = grouped.get(type_key)
    if not comp_map:
        continue
    any_entries = True
    lines.append(f"### {type_heading} (`{type_key}`)")
    lines.append("")
    for comp in sorted(comp_map.keys()):
        lines.append(f"#### `{comp}`")
        for item in comp_map[comp]:
            lines.append(item)
        lines.append("")

if not any_entries:
    lines.append("_Maintenance release with no user-facing changes._")
    lines.append("")

if prev_tag:
    compare_target = target_ref if target_ref != "HEAD" else "master"
    lines.append(f"**Full Changelog**: https://github.com/{repo}/compare/{prev_tag}...{compare_target}")

print("\n".join(lines))
PYEOF
