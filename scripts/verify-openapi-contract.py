#!/usr/bin/env python3
"""Fail when registered HTTP routes, dashboard endpoints, and core schemas drift."""

from __future__ import annotations

import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SERVER = ROOT / "internal" / "httpapi" / "server.go"
OPENAPI = ROOT / "api" / "openapi.yaml"
NAVIGATION = ROOT / "web" / "src" / "navigation.tsx"

METHODS = {"get", "post", "put", "patch", "delete"}
SCHEMA_FIELDS = {
    "RecommendationRecord": {
        "cpuThrottlingPeakRatio",
        "cpuLimitPinned",
        "spikes",
        "trends",
        "backtest",
        "dataQuality",
        "effectivePolicy",
        "provenance",
        "assessment",
    },
    "RecommendationProvenance": {
        "workloadUid",
        "workloadGeneration",
        "sourceSpecFingerprint",
        "observationStart",
        "observationEnd",
        "queryFingerprint",
        "policyFingerprint",
        "optimizerVersion",
        "expiresAt",
    },
    "Assessment": {
        "status",
        "statusReasons",
        "riskScore",
        "safetyMargins",
        "validation",
        "outOfSample",
        "checks",
        "forecast",
        "expectedSavings",
        "validForSeconds",
    },
    "Verification": {
        "recommendationId",
        "status",
        "applied",
        "postChangeWindow",
        "checks",
        "reasons",
        "checkedAt",
    },
    "Hotspot": {
        "throttlingRatio",
        "throttlingPeakRatio",
        "spikeRatio",
        "pinnedAtLimit",
        "severity",
        "issues",
        "advice",
    },
    "CorrelationResponse": {
        "status",
        "signals",
        "extra",
        "report",
        "forecast",
        "recommendation",
        "verification",
    },
}


def registered_routes(source: str) -> set[tuple[str, str]]:
    routes = {
        (match.group("method").lower(), match.group("path"))
        for match in re.finditer(
            r'mux\.HandleFunc\("(?P<method>GET|POST|PUT|PATCH|DELETE) (?P<path>[^"+]+)"',
            source,
        )
        if match.group("path") != "/api/v1/"
    }
    dynamic = re.search(
        r'for _, resource := range \[\]string\{(?P<resources>[^}]+)\}', source
    )
    if dynamic:
        for resource in re.findall(r'"([^"]+)"', dynamic.group("resources")):
            routes.add(("get", f"/api/v1/{resource}"))
    return routes


def openapi_contract(source: str) -> tuple[set[tuple[str, str]], dict[str, set[str]]]:
    routes: set[tuple[str, str]] = set()
    schemas: dict[str, set[str]] = {}
    current_path: str | None = None
    current_schema: str | None = None
    in_schemas = False
    in_properties = False

    for line in source.splitlines():
        path = re.match(r"^  (?P<path>/.*):$", line)
        if path:
            current_path = path.group("path")
            current_schema = None
            in_properties = False
            continue
        method = re.match(r"^    (?P<method>get|post|put|patch|delete):$", line)
        if current_path and method:
            routes.add((method.group("method"), current_path))

        if line == "  schemas:":
            in_schemas = True
            current_path = None
            continue
        if not in_schemas:
            continue
        schema = re.match(r"^    (?P<name>[A-Za-z][A-Za-z0-9]*):$", line)
        if schema:
            current_schema = schema.group("name")
            schemas.setdefault(current_schema, set())
            in_properties = False
            continue
        if current_schema and line == "      properties:":
            in_properties = True
            continue
        if in_properties:
            prop = re.match(r"^        (?P<name>[A-Za-z][A-Za-z0-9]*):", line)
            if prop:
                schemas[current_schema].add(prop.group("name"))
            elif re.match(r"^      \S", line):
                in_properties = False
    return routes, schemas


def dashboard_endpoints(source: str) -> set[str]:
    return set(re.findall(r"endpoint:\s*'([^']+)'", source))


def render_routes(routes: set[tuple[str, str]]) -> str:
    return "\n".join(f"  {method.upper()} {path}" for method, path in sorted(routes))


def main() -> int:
    server_routes = registered_routes(SERVER.read_text(encoding="utf-8"))
    api_routes, schemas = openapi_contract(OPENAPI.read_text(encoding="utf-8"))
    failed = False

    missing = server_routes - api_routes
    extra = api_routes - server_routes
    if missing:
        failed = True
        print("OpenAPI is missing registered routes:\n" + render_routes(missing), file=sys.stderr)
    if extra:
        failed = True
        print("OpenAPI documents routes not registered by the server:\n" + render_routes(extra), file=sys.stderr)

    paths = {path for _, path in api_routes}
    dashboard_missing = dashboard_endpoints(NAVIGATION.read_text(encoding="utf-8")) - paths
    if dashboard_missing:
        failed = True
        print(
            "Dashboard endpoints missing from OpenAPI:\n  "
            + "\n  ".join(sorted(dashboard_missing)),
            file=sys.stderr,
        )

    for schema, required_fields in SCHEMA_FIELDS.items():
        absent = required_fields - schemas.get(schema, set())
        if absent:
            failed = True
            print(
                f"Schema {schema} is missing parity fields: {', '.join(sorted(absent))}",
                file=sys.stderr,
            )

    if failed:
        return 1
    print(
        f"OpenAPI parity verified: {len(api_routes)} operations, "
        f"{len(dashboard_endpoints(NAVIGATION.read_text(encoding='utf-8')))} dashboard endpoints, "
        f"{len(SCHEMA_FIELDS)} critical schemas."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
