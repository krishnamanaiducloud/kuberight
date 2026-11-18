#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/attribution-replay.sh [--port PORT]

Backfills the synthetic workload-lifecycle fixture (rollouts, scale-down,
same-name recreation, StatefulSet replacement, missing kube-state-metrics,
autoscaling, scale-to-zero, CronJobs, resource changes) into a throwaway
Prometheus and runs the historical attribution replay against it, so the
PromQL is evaluated by Prometheus itself.

Requires prometheus and promtool on PATH, or PROMETHEUS_BIN and PROMTOOL_BIN.
The lookback delta is 90s so series that end behave as if Prometheus had
written staleness markers at scrape time.
EOF
}

port=19090
while (($#)); do
  case "$1" in
    --port)
      shift
      [[ $# -gt 0 ]] || { usage >&2; exit 2; }
      port="$1"
      ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
  shift
done

prometheus="${PROMETHEUS_BIN:-$(command -v prometheus || true)}"
promtool="${PROMTOOL_BIN:-$(command -v promtool || true)}"
[[ -x "$prometheus" && -x "$promtool" ]] || { echo "prometheus and promtool are required" >&2; exit 2; }

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
pid=""
cleanup() {
  if [[ -n "$pid" ]]; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

native() {
  # Windows binaries need native paths when this runs under Git Bash.
  if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi
}

cd "$root"
KUBERIGHT_ATTRIBUTION_FIXTURE="$(native "$work/fixture.om")" go test ./internal/usage -run '^TestWriteAttributionReplayFixture$' -count=1 >/dev/null
"$promtool" tsdb create-blocks-from openmetrics "$(native "$work/fixture.om")" "$(native "$work/data")" >/dev/null
printf 'global:\n  scrape_interval: 1m\n' >"$work/prometheus.yml"
"$prometheus" --config.file="$(native "$work/prometheus.yml")" --storage.tsdb.path="$(native "$work/data")" \
  --storage.tsdb.retention.time=100y --web.listen-address="127.0.0.1:$port" --query.lookback-delta=90s >"$work/prometheus.log" 2>&1 &
pid=$!
for _ in $(seq 1 60); do
  curl -fsS -m 2 "http://127.0.0.1:$port/-/ready" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS -m 2 "http://127.0.0.1:$port/-/ready" >/dev/null || { cat "$work/prometheus.log" >&2; exit 1; }
KUBERIGHT_TEST_PROMETHEUS_URL="http://127.0.0.1:$port" go test ./internal/usage -run '^TestAttributionReplayAgainstPrometheus$' -count=1 -v | grep -E '^ *(--- |PASS|FAIL|ok)'
