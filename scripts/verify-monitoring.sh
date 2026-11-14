#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/verify-monitoring.sh

Evaluates the chart's alert rules and the Grafana dashboard queries in
promtool:
  - renders the PrometheusRule in apply mode, checks it, and runs the unit
    tests in scripts/monitoring/alerts.test.yaml against it;
  - checks that every dashboard query parses and that no panels overlap;
  - evaluates the dashboard's savings queries against both scrape label
    layouts (honor_labels true, and the ServiceMonitor default that renames
    clashing labels to exported_*), with two replicas and a second release.

Requires helm, python3, and promtool on PATH, or PROMTOOL_BIN.
EOF
}

[[ $# -eq 0 ]] || { usage; [[ $1 == -h || $1 == --help ]] && exit 0; exit 2; }

promtool="${PROMTOOL_BIN:-$(command -v promtool || true)}"
[[ -x "$promtool" ]] || { echo "promtool is required" >&2; exit 2; }
command -v helm >/dev/null 2>&1 || { echo "helm is required" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 2; }

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

# promtool on Windows hosts (Git Bash) needs native paths.
native() {
  if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi
}

echo "== alert rules"
helm template kuberight "$repo_root/charts/kuberight" --namespace kuberight \
  --set operationMode=apply --set rbac.scope=cluster \
  --set metrics.prometheusRule.enabled=true \
  --show-only templates/monitoring.yaml >"$work/monitoring.yaml"
# The PrometheusRule spec is a promtool rule file indented by two spaces.
awk 'found { sub(/^  /, ""); print } /^spec:$/ { found = 1 }' "$work/monitoring.yaml" >"$work/rules.yaml"
grep -q 'alert: KubeRightAutomationStalled' "$work/rules.yaml" || { echo "apply-mode alerts did not render" >&2; exit 1; }
"$promtool" check rules "$(native "$work/rules.yaml")"
cp "$repo_root/scripts/monitoring/alerts.test.yaml" "$work/"
"$promtool" test rules "$(native "$work/alerts.test.yaml")"

echo "== dashboard"
python3 - "$repo_root/charts/kuberight/dashboards/kuberight.json" "$work" <<'PY'
import json
import sys

dashboard_path, work = sys.argv[1], sys.argv[2]
dashboard = json.load(open(dashboard_path, encoding="utf-8"))
substitutions = {"$namespace": "kuberight", "$__rate_interval": "5m", "$__range": "1h"}


def resolved(expr):
    for key, value in substitutions.items():
        expr = expr.replace(key, value)
    return expr


problems = []
cells = {}
queries = {}
for panel in dashboard["panels"]:
    position = panel["gridPos"]
    if position["x"] + position["w"] > 24:
        problems.append(f"{panel['title']} extends past 24 columns")
    for x in range(position["x"], position["x"] + position["w"]):
        for y in range(position["y"], position["y"] + position["h"]):
            if (x, y) in cells:
                problems.append(f"{panel['title']} overlaps {cells[(x, y)]}")
            cells[(x, y)] = panel["title"]
    for target in panel.get("targets", []):
        if target.get("datasource", {}).get("uid") != "${datasource}":
            problems.append(f"{panel['title']} does not use the datasource variable")
        queries[(panel["id"], target["refId"])] = (panel["title"], resolved(target["expr"]))
if problems:
    sys.exit("\n".join(sorted(set(problems))))


def quote(value):
    return json.dumps(value)


with open(f"{work}/dashboard-rules.yaml", "w", encoding="utf-8") as handle:
    handle.write("groups:\n  - name: dashboard\n    rules:\n")
    for (panel_id, ref), (_, expr) in sorted(queries.items()):
        handle.write(f"      - record: kuberight_dashboard:panel_{panel_id}_{ref}\n        expr: {quote(expr)}\n")

by_title = {title: expr for title, expr in queries.values()}
total, table = by_title["Estimated monthly savings"], by_title["Largest estimated savings"]

# Two replicas of this release export identical savings; a release in the
# namespace "other" must not be counted.
layouts = {
    "ServiceMonitor (clashing labels renamed to exported_*)": lambda instance, pod, release, workload, container, value: (
        f'kuberight_recommendation_monthly_savings{{namespace="{release}",exported_namespace="shop",container="kuberight",'
        f'exported_container="{container}",pod="{pod}",instance="{instance}",cluster="prod",kind="Deployment",workload="{workload}",currency="USD"}}', value),
    "annotation scrape with honor_labels": lambda instance, pod, release, workload, container, value: (
        f'kuberight_recommendation_monthly_savings{{namespace="shop",container="{container}",node="node-{pod}",'
        f'instance="{instance}",cluster="prod",kind="Deployment",workload="{workload}",currency="USD"}}', value),
}
tests = []
for name, savings in layouts.items():
    series = []
    for instance, pod, release in (("10.0.0.1:8080", "a", "kuberight"), ("10.0.0.2:8080", "b", "kuberight"), ("10.0.9.9:8080", "z", "other")):
        series.append((f'kuberight_http_requests_in_flight{{namespace="{release}",instance="{instance}",pod="{pod}"}}', "1x10"))
        series.append(savings(instance, pod, release, "api", "app", "3.5x10" if release == "kuberight" else "100x10"))
        series.append(savings(instance, pod, release, "web", "nginx", "-1.25x10" if release == "kuberight" else "100x10"))
    row = '{{cluster="prod",workload_namespace="shop",kind="Deployment",workload="{0}",container="{1}",currency="USD"}}'
    tests.append({
        "name": f"savings panels: {name}",
        "interval": "1m",
        "input_series": [{"series": key, "values": values} for key, values in series],
        "promql_expr_test": [
            {"expr": total, "eval_time": "5m", "exp_samples": [{"labels": '{currency="USD"}', "value": 2.25}]},
            {"expr": table, "eval_time": "5m", "exp_samples": [
                {"labels": row.format("api", "app"), "value": 3.5},
                {"labels": row.format("web", "nginx"), "value": -1.25},
            ]},
        ],
    })
with open(f"{work}/dashboard.test.yaml", "w", encoding="utf-8") as handle:
    json.dump({"rule_files": ["dashboard-rules.yaml"], "evaluation_interval": "1m", "tests": tests}, handle, indent=2)
print(f"{len(queries)} dashboard queries, {len(dashboard['panels'])} panels, no overlaps")
PY
"$promtool" check rules "$(native "$work/dashboard-rules.yaml")"
"$promtool" test rules "$(native "$work/dashboard.test.yaml")"
echo "monitoring verification passed"
