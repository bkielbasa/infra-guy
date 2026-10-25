#!/usr/bin/env bash
set -euo pipefail

REPO="${REPO:-bkielbasa/infra-guy}"
DEPLOY_HOST="${DEPLOY_HOST:-bklimczak@192.168.1.167}"
HOMELAB_DIR="${HOMELAB_DIR:-~/Projects/homelab-k8s}"
VERSION="${VERSION:-}"
COMMIT_MSG="${1:-}"
SKIP_TESTS="${SKIP_TESTS:-0}"

cd "$(git rev-parse --show-toplevel)"

if [[ "$SKIP_TESTS" != "1" ]]; then
  echo "==> Running tests..."
  go test ./...
fi

git fetch --tags origin

if [[ -z "$VERSION" ]]; then
  LATEST_TAG="$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n 1)"
  if [[ -z "$LATEST_TAG" ]]; then
    VERSION="v0.1.0"
  else
    IFS='.' read -r MAJOR MINOR PATCH <<< "${LATEST_TAG#v}"
    VERSION="v${MAJOR}.${MINOR}.$((PATCH + 1))"
  fi
fi

echo "==> Target version: ${VERSION}"

git add -u
git add .github .dockerignore Dockerfile cmd internal scripts go.mod go.sum 2>/dev/null || true

if ! git diff --cached --quiet; then
  MSG="${COMMIT_MSG:-chore: release ${VERSION}}"
  echo "==> Committing changes: ${MSG}"
  git commit -m "$MSG"
fi

BRANCH="$(git rev-parse --abbrev-ref HEAD)"
echo "==> Pushing ${BRANCH} to origin..."
git push origin "$BRANCH"

if git rev-parse "$VERSION" >/dev/null 2>&1; then
  echo "==> Tag ${VERSION} already exists locally"
else
  echo "==> Creating and pushing tag ${VERSION}..."
  git tag "$VERSION"
  git push origin "$VERSION"
fi

if ! gh release view "$VERSION" --repo "$REPO" >/dev/null 2>&1; then
  echo "==> Generating grouped release notes for ${VERSION}..."
  NOTES_FILE="$(mktemp)"
  ./scripts/release-notes.sh "$VERSION" > "$NOTES_FILE"
  echo "==> Creating GitHub release ${VERSION}..."
  gh release create "$VERSION" --repo "$REPO" --title "$VERSION" --notes-file "$NOTES_FILE"
  rm -f "$NOTES_FILE"
fi

echo "==> locating GitHub Actions Release workflow run for ${VERSION}..."
RUN_ID=""
for _ in {1..20}; do
  RUN_ID="$(gh run list --repo "$REPO" --workflow=release.yml --branch "$VERSION" --limit 1 --json databaseId -q '.[0].databaseId' 2>/dev/null || true)"
  if [[ -n "$RUN_ID" && "$RUN_ID" != "null" ]]; then
    break
  fi
  sleep 3
done

if [[ -z "$RUN_ID" || "$RUN_ID" == "null" ]]; then
  echo "ERROR: Could not find Release workflow run for ${VERSION}" >&2
  exit 1
fi

echo "==> Waiting for workflow run ${RUN_ID} to complete..."
while true; do
  STATE="$(gh run view "$RUN_ID" --repo "$REPO" --json status,conclusion -q '.status + ":" + (.conclusion // "")' 2>/dev/null || echo "unknown:")"
  STATUS="${STATE%%:*}"
  CONCLUSION="${STATE#*:}"
  if [[ "$STATUS" == "completed" ]]; then
    if [[ "$CONCLUSION" == "success" ]]; then
      echo "==> Image build succeeded (run ${RUN_ID})"
      break
    else
      echo "ERROR: Workflow run ${RUN_ID} finished with conclusion=${CONCLUSION}" >&2
      exit 1
    fi
  fi
  echo "    [${VERSION}] workflow status: ${STATUS}..."
  sleep 10
done

echo "==> Updating homelab-k8s on ${DEPLOY_HOST} and syncing ArgoCD..."
ssh "$DEPLOY_HOST" "bash -ec '
  cd ${HOMELAB_DIR}
  git pull --rebase
  grep -rlE \"v[0-9]+\\.[0-9]+\\.[0-9]+\" helm/infra-guy values/infra-guy.yaml | xargs sed -i -E \"s/v[0-9]+\\.[0-9]+\\.[0-9]+/${VERSION}/g\"
  if ! git diff --quiet; then
    git commit -am \"chore(infra-guy): bump to ${VERSION}\"
    git push
  fi
  kubectl annotate application infra-guy -n argocd argocd.argoproj.io/refresh=hard --overwrite
  for _ in {1..60}; do
    IMG=\$(kubectl get deployment infra-guy -n infra-guy -o jsonpath=\"{.spec.template.spec.containers[0].image}\" 2>/dev/null || true)
    if [[ \"\$IMG\" == \"ghcr.io/${REPO}:${VERSION}\" ]]; then
      break
    fi
    sleep 2
  done
  kubectl rollout status -n infra-guy deployment/infra-guy --timeout=180s
  echo \"Deployed image: \$(kubectl get deployment infra-guy -n infra-guy -o jsonpath=\"{.spec.template.spec.containers[0].image}\")\"
  kubectl get pods -n infra-guy -o wide
'"

echo "==> Successfully deployed ${VERSION}!"
