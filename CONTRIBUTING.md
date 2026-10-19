# Contributing to KubeRight

KubeRight is a development preview. Contributions should preserve its fail-closed behavior and must not turn an unavailable integration into invented data.

## Development setup

Use Go 1.27, Python 3.12, Node.js 24, npm, and Helm 4.3.1. The CI workflow is the authoritative list of required checks.

```sh
go mod download
python -m venv .venv
. .venv/bin/activate
python -m pip install -r ml/requirements-dev.txt
npm ci --prefix web
```

Run the relevant checks before opening a pull request:

```sh
gofmt -w cmd internal
go vet ./...
go test -race ./...

python -m ruff format --check ml
python -m ruff check ml
python -m mypy --config-file ml/pyproject.toml ml
python -m pytest ml --cov=ml

npm run lint --prefix web
npm run typecheck --prefix web
npm test --prefix web
npm run build --prefix web

helm lint --strict charts/kuberight
scripts/render-manifests.sh --check
```

On Windows, run the manifest script from Git Bash or WSL. Container, vulnerability, Kubernetes schema, and Kind checks also run in CI.

## Change guidelines

- Add or update tests for behavior changes, including denied and dependency-failure paths for security-sensitive code.
- Update `api/openapi.yaml` and [the API reference](docs/api.md) when the Go HTTP contract changes.
- Keep `go.sum`, `web/package-lock.json`, and `ml/requirements.lock` synchronized with dependency changes.
- Change Helm templates and `values.schema.json` together. Regenerate `deploy/manifests/*.yaml` with `scripts/render-manifests.sh`; never edit generated manifests directly.
- Describe only implemented and verified behavior. Add unfinished work to [known limitations](docs/known-limitations.md).
- Never commit credentials, kubeconfigs, model data, production metrics, or customer identifiers.
- Keep default access read-only and namespace-scoped. A change that expands permissions needs an explicit threat and migration analysis in the pull request.

Use a focused branch and pull request. Explain the user-visible result, risk, tests run, compatibility impact, and any follow-up work. Report vulnerabilities through [SECURITY.md](SECURITY.md), not through a public issue.
