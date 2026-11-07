#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/render-manifests.sh [--check] [--namespace NAME]

Renders deterministic default, production, and OpenShift standalone manifests
from the Helm chart. --check exits non-zero when committed output is stale.
EOF
}

check=false
namespace=kuberight
while (($#)); do
  case "$1" in
    --check) check=true ;;
    --namespace)
      shift
      [[ $# -gt 0 ]] || { usage >&2; exit 2; }
      namespace=$1
      ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
  shift
done

command -v helm >/dev/null 2>&1 || {
  echo "helm is required (https://helm.sh/docs/intro/install/)" >&2
  exit 1
}

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
chart="$repo_root/charts/kuberight"
destination="$repo_root/deploy/manifests"
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT

render() {
  local name=$1
  shift
  helm template kuberight "$chart" \
    --namespace "$namespace" \
    --set namespace.create=true \
    "$@" >"$tmp/$name.yaml"
}

render kuberight
render kuberight-production -f "$chart/profiles/production.yaml"
render kuberight-openshift -f "$chart/profiles/openshift.yaml"

if $check; then
  status=0
  for manifest in "$tmp"/*.yaml; do
    target="$destination/$(basename "$manifest")"
    if [[ ! -f "$target" ]] || ! cmp -s "$manifest" "$target"; then
      echo "stale standalone manifest: ${target#"$repo_root/"}" >&2
      status=1
    fi
  done
  exit "$status"
fi

mkdir -p "$destination"
for manifest in "$tmp"/*.yaml; do
  cp "$manifest" "$destination/$(basename "$manifest")"
done
echo "Rendered standalone manifests in ${destination#"$repo_root/"}/"
