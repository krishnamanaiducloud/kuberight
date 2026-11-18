import type { ApiRecord, RecommendationProvenance, RecommendationRecord } from '../api'

export type EvidenceRecord = {
  source?: RecommendationRecord['source']
  createdAt?: string
  cluster?: string
  provenance?: RecommendationProvenance
  dataQuality?: RecommendationRecord['dataQuality']
}

export type EvidenceState = {
  kind: 'fresh' | 'expiring' | 'expired' | 'caller' | 'unverified'
  label: string
  detail: string
  canApprove: boolean
  color: 'success' | 'warning' | 'error'
}

const verifiable = (provenance?: RecommendationProvenance) => Boolean(
  provenance?.workloadUid &&
  provenance.workloadGeneration && provenance.workloadGeneration > 0 &&
  provenance.sourceSpecFingerprint &&
  provenance.expiresAt,
)

/** Mirrors the server's approval prerequisites while keeping caller samples explicitly manual. */
export function recommendationEvidenceState(record: EvidenceRecord, now = Date.now()): EvidenceState {
  if (record.source !== 'prometheus') {
    return {
      kind: 'caller',
      label: 'Caller-provided evidence',
      detail: 'KubeRight did not collect these samples or bind them to a live workload. Verify the source and current workload before approval.',
      canApprove: true,
      color: 'warning',
    }
  }
  if (!verifiable(record.provenance)) {
    return {
      kind: 'unverified',
      label: 'Live provenance unavailable',
      detail: 'This Prometheus recommendation is not bound to a verifiable workload generation. Generate a new recommendation before approval.',
      canApprove: false,
      color: 'error',
    }
  }
  const expiresAt = Date.parse(record.provenance!.expiresAt!)
  if (!Number.isFinite(expiresAt)) {
    return {
      kind: 'unverified',
      label: 'Invalid evidence expiry',
      detail: 'The validity timestamp cannot be verified. Generate a new recommendation before approval.',
      canApprove: false,
      color: 'error',
    }
  }
  const remaining = expiresAt - now
  if (remaining <= 0) {
    return {
      kind: 'expired',
      label: 'Evidence expired',
      detail: 'The observation window is no longer valid. Generate a new recommendation from current workload and metrics data.',
      canApprove: false,
      color: 'error',
    }
  }
  if (remaining <= 2 * 60 * 60 * 1000) {
    return {
      kind: 'expiring',
      label: 'Evidence expires soon',
      detail: 'Review promptly. The server will recheck the live workload identity and specification before recording approval.',
      canApprove: true,
      color: 'warning',
    }
  }
  return {
    kind: 'fresh',
    label: 'Evidence valid',
    detail: 'Bound to a workload generation and observation window. The server rechecks the live workload before recording approval.',
    canApprove: true,
    color: 'success',
  }
}

/** Safely adapts a generic API row for the recommendation evidence UI. */
export function evidenceRecordFromRow(row: ApiRecord): EvidenceRecord {
  return row as unknown as EvidenceRecord
}
