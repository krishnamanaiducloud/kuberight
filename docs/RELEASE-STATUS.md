# Release status

KubeRight is an **incomplete development preview**. The repository import is in progress.

The source tree is not yet complete. Do not use the repository for a deployment or publish a Docker Hub image.

Known blockers:
- go.sum is missing; reproducible Go dependencies cannot be verified.
- API, authentication, Kubernetes integration, Helm templates and test files are not all imported.
- End-to-end Kubernetes and OpenShift compatibility is unverified.
- CI is intentionally gated; no image publishing workflow is enabled.

The full development source archive is held by the project owner. A subsequent import must reconcile all files before declaring the repository ready.
