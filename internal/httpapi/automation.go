package httpapi

import "net/http"

// automationRemoved answers the endpoints of the removed apply mode.
// KubeRight is read-only: it never changes a workload. The routes stay so
// clients of the published API get a clear answer instead of a 404.
const automationRemoved = "KubeRight is read-only and never changes workloads; apply approved recommendations through a GitOps pull request or your own deployment process"

func (server *apiServer) automationStatus(w http.ResponseWriter, r *http.Request) {
	respondError(w, r, http.StatusGone, "automation_removed", automationRemoved)
}

func (server *apiServer) runAutomation(w http.ResponseWriter, r *http.Request) {
	respondError(w, r, http.StatusGone, "automation_removed", automationRemoved)
}
