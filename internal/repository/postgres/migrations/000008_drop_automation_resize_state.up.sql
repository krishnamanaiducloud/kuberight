-- Apply mode was removed: KubeRight no longer changes workloads, so the
-- state of in-place resizes it started is no longer kept.
DROP TABLE IF EXISTS automation_resize_state;
