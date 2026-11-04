import type { ReactNode } from 'react'
import { Alert, AlertTitle, Box, Chip, Stack, Tooltip, Typography } from '@mui/material'
import type { RecommendationRecord } from '../api'
import { formatDate } from '../format'
import { recommendationEvidenceState, type EvidenceRecord } from './recommendationEvidenceState'

type EvidenceSource = RecommendationRecord['source']

/** Compact evidence status for recommendation and approval tables. */
export function EvidenceStatusChip({ record, now }: { record: EvidenceRecord; now?: number }) {
  const state = recommendationEvidenceState(record, now)
  const expiresAt = record.provenance?.expiresAt
  const title = expiresAt && (state.kind === 'fresh' || state.kind === 'expiring')
    ? `${state.detail} Valid until ${formatDate(expiresAt)}.`
    : state.detail
  return (
    <Tooltip title={title}>
      <Chip size="small" variant="outlined" color={state.color} label={state.label} />
    </Tooltip>
  )
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Box>
      <Typography variant="caption" color="text.secondary" component="dt">{label}</Typography>
      <Typography variant="body2" component="dd" sx={{ m: 0, overflowWrap: 'anywhere' }}>{children}</Typography>
    </Box>
  )
}

/** Human-readable evidence window plus the immutable identifiers needed for audit. */
export function RecommendationEvidence({ record, source, compact = false, now }: {
  record: EvidenceRecord
  source?: EvidenceSource
  compact?: boolean
  now?: number
}) {
  const normalized = { ...record, source: source ?? record.source }
  const state = recommendationEvidenceState(normalized, now)
  const provenance = normalized.provenance
  const quality = normalized.dataQuality
  const replicas = quality?.replicaCoverage
  return (
    <Box>
      <Typography variant="subtitle2" sx={{ mb: 0.75 }}>Evidence &amp; freshness</Typography>
      <Alert severity={state.color} variant="outlined">
        <AlertTitle>{state.label}</AlertTitle>
        {state.detail}
      </Alert>
      <Box component="dl" sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', sm: compact ? 'repeat(2, minmax(0, 1fr))' : 'repeat(3, minmax(0, 1fr))' }, gap: 1.25, mt: 1.25, mb: 0 }}>
        {provenance?.observationStart && provenance.observationEnd && (
          <Fact label="Observation window">{formatDate(provenance.observationStart)} – {formatDate(provenance.observationEnd)}</Fact>
        )}
        {provenance?.expiresAt && <Fact label="Valid until"><time dateTime={provenance.expiresAt}>{formatDate(provenance.expiresAt)}</time></Fact>}
        {quality && <Fact label="Accepted samples">CPU {quality.cpuSampleCount.toLocaleString()} · memory {quality.memorySampleCount.toLocaleString()}</Fact>}
        {replicas && <Fact label="Replica evidence">{replicas.observedReplicas} of {replicas.expectedReplicas} observed · {replicas.complete ? 'complete' : 'incomplete'}</Fact>}
        {replicas && <Fact label="Temporal coverage">CPU {Math.round(replicas.cpuTemporalCoverage * 100)}% · memory {Math.round(replicas.memoryTemporalCoverage * 100)}%</Fact>}
        {provenance?.workloadGeneration !== undefined && <Fact label="Workload identity">Generation {provenance.workloadGeneration}{record.cluster ? ` · ${record.cluster}` : ''}</Fact>}
        {provenance?.optimizerVersion && <Fact label="Optimizer">{provenance.optimizerVersion}</Fact>}
        {record.createdAt && <Fact label="Generated">{formatDate(record.createdAt)}</Fact>}
      </Box>
      {!compact && provenance && (
        <Stack direction="row" gap={0.75} flexWrap="wrap" sx={{ mt: 1.25 }} aria-label="Technical provenance">
          {provenance.workloadUid && <Chip size="small" variant="outlined" label={`UID ${provenance.workloadUid}`} sx={{ maxWidth: '100%' }} />}
          {provenance.workloadResourceVersion && <Chip size="small" variant="outlined" label={`Resource version ${provenance.workloadResourceVersion}`} />}
          {provenance.sourceSpecFingerprint && <Chip size="small" variant="outlined" label={`Spec ${provenance.sourceSpecFingerprint.slice(0, 12)}…`} />}
          {provenance.queryFingerprint && <Chip size="small" variant="outlined" label={`Query ${provenance.queryFingerprint.slice(0, 12)}…`} />}
          {provenance.policyFingerprint && <Chip size="small" variant="outlined" label={`Policy ${provenance.policyFingerprint.slice(0, 12)}…`} />}
        </Stack>
      )}
      {!compact && replicas && (
        <Stack direction="row" gap={0.75} flexWrap="wrap" sx={{ mt: 1.25 }} aria-label="Replica completeness">
          <Chip size="small" variant="outlined" color={replicas.complete ? 'success' : 'error'} label={replicas.complete ? 'All expected replicas covered' : `${replicas.missingReplicas} replica${replicas.missingReplicas === 1 ? '' : 's'} missing`} />
          <Chip size="small" variant="outlined" label={`CPU population ${Math.round(replicas.cpuReplicaCoverage * 100)}%`} />
          <Chip size="small" variant="outlined" label={`Memory population ${Math.round(replicas.memoryReplicaCoverage * 100)}%`} />
          {replicas.cpuImbalanceRatio !== undefined && replicas.cpuImbalanceRatio > 0 && <Chip size="small" variant="outlined" label={`CPU busiest/quietest P95 ${replicas.cpuImbalanceRatio.toFixed(2)}×`} />}
          {replicas.memoryImbalanceRatio !== undefined && replicas.memoryImbalanceRatio > 0 && <Chip size="small" variant="outlined" label={`Memory busiest/quietest P95 ${replicas.memoryImbalanceRatio.toFixed(2)}×`} />}
        </Stack>
      )}
    </Box>
  )
}
