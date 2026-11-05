import type { ReactNode } from 'react'
import { Alert, AlertTitle, Box, Chip, Stack, Tooltip, Typography } from '@mui/material'
import type { RecommendationRecord } from '../api'
import { coverageGaps, formatDate } from '../format'
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
        {replicas && <Fact label="Replica evidence">{replicas.observedReplicas} of {replicas.discoveredReplicas} attributed pods observed · {replicas.complete ? 'complete' : 'incomplete'}</Fact>}
        {replicas?.attribution && (
          <Fact label="Attribution">
            {replicas.attribution === 'historical' ? 'Every pod in the window, including replaced ones' : 'Current pods only'}
            {replicas.revisions ? ` · ${replicas.revisions} revision${replicas.revisions === 1 ? '' : 's'}` : ''}
          </Fact>
        )}
        {replicas?.peakExpectedReplicas !== undefined && replicas.peakExpectedReplicas > 0 && (
          <Fact label="Desired replicas">
            {replicas.minimumExpectedReplicas === replicas.peakExpectedReplicas ? replicas.peakExpectedReplicas : `${replicas.minimumExpectedReplicas ?? 0}–${replicas.peakExpectedReplicas}`}
            {replicas.scalingEvents ? ` · ${replicas.scalingEvents} scaling change${replicas.scalingEvents === 1 ? '' : 's'}` : ''}
          </Fact>
        )}
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
          <Tooltip title="Share of expected replica-time, summed over every step, with samples">
            <Chip size="small" variant="outlined" label={`CPU replica-time ${Math.floor(replicas.cpuReplicaCoverage * 100)}%`} />
          </Tooltip>
          <Tooltip title="Share of expected replica-time, summed over every step, with samples">
            <Chip size="small" variant="outlined" label={`Memory replica-time ${Math.floor(replicas.memoryReplicaCoverage * 100)}%`} />
          </Tooltip>
          {replicas.identityCoverage !== undefined && <Chip size="small" variant="outlined" label={`Attributed identity ${Math.floor(replicas.identityCoverage * 100)}%`} />}
          {replicas.scaledToZeroSteps ? <Chip size="small" variant="outlined" label={`Scaled to zero for ${replicas.scaledToZeroSteps} steps`} /> : null}
          {replicas.cpuImbalanceRatio !== undefined && replicas.cpuImbalanceRatio > 0 && <Chip size="small" variant="outlined" label={`CPU busiest/quietest P95 ${replicas.cpuImbalanceRatio.toFixed(2)}×`} />}
          {replicas.memoryImbalanceRatio !== undefined && replicas.memoryImbalanceRatio > 0 && <Chip size="small" variant="outlined" label={`Memory busiest/quietest P95 ${replicas.memoryImbalanceRatio.toFixed(2)}×`} />}
        </Stack>
      )}
      {!compact && <CoverageGaps replicas={replicas} />}
    </Box>
  )
}

/** Names the time ranges in which expected replicas went unobserved. */
function CoverageGaps({ replicas }: { replicas?: NonNullable<EvidenceRecord['dataQuality']>['replicaCoverage'] }) {
  const gaps = coverageGaps(replicas?.timeline)
  if (!replicas || gaps.length === 0) return null
  const shown = gaps.slice(0, 5)
  return (
    <Box sx={{ mt: 1.25 }}>
      <Typography variant="caption" color="text.secondary" component="p">Coverage below 90% of expected replicas</Typography>
      <Box component="ul" sx={{ m: 0, pl: 2.5 }} aria-label="Coverage gaps">
        {shown.map((gap) => (
          <Typography component="li" variant="body2" key={gap.start}>
            {formatDate(gap.start)} – {formatDate(gap.end)}: CPU {Math.floor(gap.cpu * 100)}%, memory {Math.floor(gap.memory * 100)}%
          </Typography>
        ))}
      </Box>
      {gaps.length > shown.length && <Typography variant="caption" color="text.secondary">{gaps.length - shown.length} more ranges</Typography>}
    </Box>
  )
}
