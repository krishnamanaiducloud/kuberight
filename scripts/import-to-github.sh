#!/usr/bin/env bash
set -euo pipefail
repo="https://github.com/krishnamanaiducloud/kuberight.git"
if [[ ! -f go.mod || ! -d internal || ! -d charts/kuberight ]]; then
  echo 'Run from extracted kuberight/ source directory.' >&2; exit 1
fi
if ! command -v git >/dev/null || ! command -v go >/dev/null; then
  echo 'git and Go are required.' >&2; exit 1
fi
source_dir="$PWD"
if [[ -d "$source_dir/.git" ]]; then
  echo 'Source directory must not contain .git metadata' >&2; exit 1
fi
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
git clone "$repo" "$work/repo"
cd "$work/repo"
git checkout -b "import/full-source-$(date +%Y%m%d%H%M%S)"
cp -a "$source_dir"/. .
if [[ -f go.sum ]]; then go mod verify; else go mod tidy; fi
go test ./...
go vet ./...
go build ./cmd/kuberight
python3 -m pytest -q ml
if command -v helm >/dev/null; then
  helm lint charts/kuberight
  helm template kuberight charts/kuberight >/dev/null
else
  echo 'Helm not installed; PR CI must verify Helm.'
fi
git add -A
git commit -m 'feat: import complete KubeRight reviewed source'
git push -u origin HEAD
echo 'Review the branch, open a pull request, and require passing CI before merging.'
