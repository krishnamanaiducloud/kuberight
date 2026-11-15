#!/usr/bin/env python3
"""Report breaking changes between two versions of api/openapi.yaml.

Usage:
    git show v0.1.0:api/openapi.yaml > /tmp/base.yaml
    scripts/check-openapi-compat.py /tmp/base.yaml api/openapi.yaml

Within /api/v1, a change is breaking when it removes an operation, a
parameter, a schema, a schema property, or an enum value; makes a parameter
or a property required that was optional; or changes a property's type.
Additions are compatible. The exit status is 1 when anything breaks.
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

import yaml

METHODS = ("get", "put", "post", "delete", "patch")


def load(path: str) -> dict[str, Any]:
    with Path(path).open(encoding="utf-8") as stream:
        document = yaml.safe_load(stream)
    if not isinstance(document, dict) or "paths" not in document:
        raise SystemExit(f"{path} is not an OpenAPI document")
    return document


def parameters(operation: dict[str, Any]) -> dict[tuple[str, str], bool]:
    found = {}
    for parameter in operation.get("parameters", []):
        if "name" in parameter:
            found[(parameter["in"], parameter["name"])] = bool(parameter.get("required"))
    return found


def compare_schema(name: str, base: dict[str, Any], head: dict[str, Any], problems: list[str]) -> None:
    if base.get("type") and head.get("type") and base["type"] != head["type"]:
        problems.append(f"schema {name}: type changed from {base['type']} to {head['type']}")
    removed_values = set(map(str, base.get("enum", []))) - set(map(str, head.get("enum", [])))
    if head.get("enum") is not None and removed_values:
        problems.append(f"schema {name}: enum values removed: {', '.join(sorted(removed_values))}")
    base_properties, head_properties = base.get("properties", {}), head.get("properties", {})
    for prop in sorted(set(base_properties) - set(head_properties)):
        problems.append(f"schema {name}: property {prop} removed")
    for prop in sorted(set(head.get("required", [])) - set(base.get("required", []))):
        if prop in base_properties:
            problems.append(f"schema {name}: property {prop} became required")
        elif head_properties.get(prop) is not None:
            problems.append(f"schema {name}: new required property {prop}")
    for prop in sorted(set(base_properties) & set(head_properties)):
        if isinstance(base_properties[prop], dict) and isinstance(head_properties[prop], dict):
            compare_schema(f"{name}.{prop}", base_properties[prop], head_properties[prop], problems)


def breaking_changes(base: dict[str, Any], head: dict[str, Any]) -> list[str]:
    problems: list[str] = []
    for path, base_item in base["paths"].items():
        if not path.startswith("/api/v1"):
            continue
        head_item = head["paths"].get(path)
        for method in METHODS:
            if method not in base_item:
                continue
            if head_item is None or method not in head_item:
                problems.append(f"{method.upper()} {path} removed")
                continue
            base_parameters, head_parameters = parameters(base_item[method]), parameters(head_item[method])
            for key in sorted(set(base_parameters) - set(head_parameters)):
                problems.append(f"{method.upper()} {path}: {key[0]} parameter {key[1]} removed")
            for key, required in sorted(head_parameters.items()):
                if required and not base_parameters.get(key, False):
                    problems.append(f"{method.upper()} {path}: {key[0]} parameter {key[1]} is now required")
    base_schemas = base.get("components", {}).get("schemas", {})
    head_schemas = head.get("components", {}).get("schemas", {})
    for name in sorted(base_schemas):
        if name not in head_schemas:
            problems.append(f"schema {name} removed")
        else:
            compare_schema(name, base_schemas[name], head_schemas[name], problems)
    return problems


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    problems = breaking_changes(load(argv[1]), load(argv[2]))
    for problem in problems:
        print(f"breaking: {problem}")
    if not problems:
        print("No breaking changes to /api/v1.")
    return 1 if problems else 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
