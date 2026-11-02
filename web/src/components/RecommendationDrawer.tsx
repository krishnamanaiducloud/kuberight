import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert, Box, Button, Chip, CircularProgress, Divider, Drawer, IconButton, Stack, Table, TableBody,
  TableCell, TableHead, TableRow, TextField, ToggleButton, ToggleButtonGroup, Tooltip, Typography,
} from '@mui/material'
import CloseIcon from '@mui/icons-material/Close'
import ContentCopyIcon from '@mui/icons-material/ContentCopy'
import { api, ApiError, type RecommendationRecord } from '../api'
import { formatBytes, formatDate, formatMillicores, kubectlCommand, yamlPatch } from '../format'
import { ResourceTable } from './ResourceTable'
import { BacktestTable, CpuBehaviorChips, TrendChips } from './Backtest'
import { RecommendationEvidence } from './RecommendationEvidence'
import { AssessmentPanel, AssessmentStatusChip } from './AssessmentPanel'
import { recommendationEvidenceState } from './recommendationEvidenceState'

const riskColor = { low: 'success', medium: 'warning', high: 'error' } as const
const reviewColor = { pending: 'warning', approved: 'success', rejected: 'default' } as const

const riskGuidance = {
  low: { severity: 'success', text: 'Low modeled risk. Confirm policy, ownership, and rollout timing before applying.' },
  medium: { severity: 'warning', text: 'Material safeguard or workload uncertainty exists. Validate under representative load before applying.' },
  high: { severity: 'error', text: 'High-risk recommendation. Validate in a canary or non-production environment before approval.' },
} as const

function ReviewPanel({ record }: { record: RecommendationRecord }) {
  const client = useQueryClient()
  const [comment, setComment] = useState('')
  const evidence = recommendationEvidenceState(record)
  // Hold, insufficient-data, and unsupported advice can be rejected but not approved.
  const verdictBlocks = record.assessment !== undefined && record.assessment.status !== 'recommend'
  const mutation = useMutation({
    mutationFn: (decision: 'approve' | 'reject') => api.post<RecommendationRecord>(`/api/v1/recommendations/${record.id}/review`, { decision, comment }),
    onSuccess: (updated) => {
      client.setQueryData(['recommendation', record.id], updated)
      void client.invalidateQueries({ queryKey: ['resource'] })
      void client.invalidateQueries({ queryKey: ['overview'] })
    },
  })
  const error = mutation.error instanceof ApiError ? mutation.error.message : mutation.error ? 'The review could not be saved.' : undefined
  const errorSeverity = mutation.error instanceof ApiError && [409, 503].includes(mutation.error.status) ? 'warning' : 'error'
  return (
    <Stack gap={1.5}>
      <Typography variant="body2" color="text.secondary">Approval records a decision for your change process. KubeRight never applies the change.</Typography>
      <TextField size="small" multiline minRows={2} label="Comment" helperText="Required when rejecting" value={comment} onChange={(event) => setComment(event.target.value)} inputProps={{ maxLength: 1024 }} />
      {!evidence.canApprove && <Alert severity="error">Approval is disabled: {evidence.detail}</Alert>}
      {evidence.canApprove && verdictBlocks && <Alert severity="warning">Approval is disabled: the assessment status is {record.assessment!.status.replace('_', ' ')}. {record.assessment!.statusReasons.join(' ')}</Alert>}
      {error && <Alert severity={errorSeverity}>{error}</Alert>}
      <Stack direction="row" gap={1}>
        <Button variant="contained" color="success" disabled={mutation.isPending || !evidence.canApprove || verdictBlocks} onClick={() => mutation.mutate('approve')}>Approve</Button>
        <Button variant="outlined" color="error" disabled={mutation.isPending || comment.trim() === ''} onClick={() => mutation.mutate('reject')}>Reject</Button>
        {mutation.isPending && <CircularProgress size={22} sx={{ alignSelf: 'center' }} />}
      </Stack>
    </Stack>
  )
}

function ApplyCommand({ record }: { record: RecommendationRecord }) {
  const command = kubectlCommand(record)
  const [format, setFormat] = useState<'kubectl' | 'yaml'>('kubectl')
  const [copied, setCopied] = useState(false)
  if (!command) return null
  const content = format === 'kubectl' ? command : yamlPatch([record])
  return (
    <Box>
      <Stack direction="row" alignItems="center" justifyContent="space-between" sx={{ mb: 0.5 }}>
        <Typography variant="subtitle2">Apply through your change process</Typography>
        <ToggleButtonGroup size="small" exclusive value={format} onChange={(_, next) => { if (next) { setFormat(next); setCopied(false) } }} aria-label="Change format">
          <ToggleButton value="kubectl">kubectl</ToggleButton>
          <ToggleButton value="yaml">YAML patch</ToggleButton>
        </ToggleButtonGroup>
      </Stack>
      <Box component="pre" sx={{ position: 'relative', m: 0, p: 1.5, pr: 5, borderRadius: 1.5, bgcolor: '#091621', border: '1px solid', borderColor: 'divider', fontFamily: 'ui-monospace, SFMono-Regular, Consolas, monospace', fontSize: 12.5, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
        {content}
        <Tooltip title={copied ? 'Copied' : 'Copy'}>
          <IconButton size="small" aria-label="Copy change" sx={{ position: 'absolute', top: 6, right: 6 }} onClick={() => { void navigator.clipboard?.writeText(content).then(() => setCopied(true)) }}>
            <ContentCopyIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      </Box>
    </Box>
  )
}

export function RecommendationDrawer({ id, onClose }: { id: string | undefined; onClose: () => void }) {
  const query = useQuery({
    queryKey: ['recommendation', id],
    queryFn: ({ signal }) => api.get<RecommendationRecord>(`/api/v1/recommendations/${id}`, { signal }),
    enabled: Boolean(id),
  })
  const record = query.data
  return (
    <Drawer anchor="right" open={Boolean(id)} onClose={onClose} PaperProps={{ 'aria-labelledby': 'recommendation-title', sx: { width: { xs: '100%', sm: 600 }, bgcolor: 'background.paper' } }}>
      <Stack direction="row" alignItems="center" sx={{ p: 2, pb: 1 }}>
        <Typography id="recommendation-title" variant="h6" sx={{ flexGrow: 1 }}>Recommendation decision</Typography>
        <IconButton aria-label="Close recommendation" onClick={onClose}><CloseIcon /></IconButton>
      </Stack>
      <Divider />
      <Box sx={{ p: 2, overflowY: 'auto' }}>
        {query.isLoading && <CircularProgress />}
        {query.error && <Alert severity="error">{query.error instanceof Error ? query.error.message : 'The recommendation could not be loaded.'}</Alert>}
        {record && (
          <Stack gap={2.5}>
            <Box>
              <Typography variant="h5" sx={{ wordBreak: 'break-word' }}>{record.workload} / {record.container}</Typography>
              <Typography color="text.secondary">{[record.cluster, record.namespace, record.kind].filter(Boolean).join(' · ')}{record.replicas ? ` · ${record.replicas} replica${record.replicas === 1 ? '' : 's'}` : ''}</Typography>
              <Stack direction="row" gap={1} flexWrap="wrap" sx={{ mt: 1.5 }}>
                {record.assessment && <AssessmentStatusChip status={record.assessment.status} />}
                <Chip size="small" label={`Review: ${record.reviewStatus}`} color={reviewColor[record.reviewStatus]} variant="outlined" />
                <Chip size="small" label={`Risk: ${record.risk}`} color={riskColor[record.risk]} variant="outlined" />
                <Chip size="small" label={`Confidence: ${record.confidence} (${Math.round(record.confidenceScore * 100)}%)`} variant="outlined" />
                <Chip size="small" label={record.source === 'prometheus' ? 'From Prometheus' : 'Caller samples'} variant="outlined" />
                {record.burstable && (!record.spikes || record.spikes.classification === 'none') && <Chip size="small" label="Bursty CPU" color="warning" variant="outlined" />}
              </Stack>
            </Box>
            <RecommendationEvidence record={record} />
            {record.assessment && <AssessmentPanel assessment={record.assessment} />}
            <ResourceTable record={record} />
            <CpuBehaviorChips throttling={record.cpuThrottlingRatio} throttlingPeak={record.cpuThrottlingPeakRatio} pinned={record.cpuLimitPinned} spikes={record.spikes} />
            <TrendChips trends={record.trends} />
            {record.backtest && <BacktestTable backtest={record.backtest} />}
            <Box>
              <Typography variant="subtitle2">Why these values</Typography>
              <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>{record.reason}</Typography>
              <Alert severity={riskGuidance[record.risk].severity} variant="outlined" sx={{ mt: 1 }}>{riskGuidance[record.risk].text}</Alert>
            </Box>

            <Box>
              <Typography variant="subtitle2" sx={{ mb: 0.5 }}>Evidence used</Typography>
              <Table size="small" aria-label="Usage percentiles">
                <TableHead><TableRow><TableCell /><TableCell align="right">P50</TableCell><TableCell align="right">P95</TableCell><TableCell align="right">P99</TableCell><TableCell align="right">Max</TableCell></TableRow></TableHead>
                <TableBody>
                  <TableRow><TableCell>CPU</TableCell>{(['p50', 'p95', 'p99', 'max'] as const).map((key) => <TableCell key={key} align="right">{formatMillicores(record.cpuStatisticsMilli[key])}</TableCell>)}</TableRow>
                  <TableRow><TableCell>Memory</TableCell>{(['p50', 'p95', 'p99', 'max'] as const).map((key) => <TableCell key={key} align="right">{formatBytes(record.memoryStatisticsBytes[key])}</TableCell>)}</TableRow>
                </TableBody>
              </Table>
              <Typography variant="caption" color="text.secondary">
                {record.dataQuality.cpuSampleCount} CPU and {record.dataQuality.memorySampleCount} memory samples; {record.dataQuality.minimumRequired} required for reductions.
              </Typography>
              <Stack direction="row" gap={1} flexWrap="wrap" sx={{ mt: 1 }}>
                <Chip size="small" color={record.dataQuality.sufficient ? 'success' : 'warning'} variant="outlined" label={record.dataQuality.sufficient ? 'Sample threshold met' : 'Sample threshold not met'} />
                <Chip size="small" variant="outlined" label={record.source === 'prometheus' ? 'Metrics: Prometheus' : 'Metrics: caller supplied'} />
              </Stack>
            </Box>

            {((record.dataQuality.issues ?? []).length > 0 || (record.warnings ?? []).length > 0) && (
              <Stack gap={1}>
                {(record.dataQuality.issues ?? []).map((issue) => <Alert key={`q-${issue}`} severity="warning" variant="outlined">{issue}</Alert>)}
                {(record.warnings ?? []).map((warning) => <Alert key={`w-${warning}`} severity="info" variant="outlined">{warning}</Alert>)}
              </Stack>
            )}

            <Box sx={{ p: 1.5, border: '1px solid', borderColor: 'divider', borderRadius: 2, bgcolor: 'rgba(255,255,255,.015)' }}>
              <Typography variant="subtitle2" sx={{ mb: 1 }}>Audit record</Typography>
              <Box component="dl" sx={{ display: 'grid', gridTemplateColumns: 'max-content minmax(0, 1fr)', columnGap: 2, rowGap: 0.75, m: 0, '& dt': { color: 'text.secondary' }, '& dd': { m: 0, overflowWrap: 'anywhere' } }}>
                <Typography component="dt" variant="caption">Generator</Typography><Typography component="dd" variant="caption">{record.createdBy || 'Not reported'}</Typography>
                <Typography component="dt" variant="caption">Evidence source</Typography><Typography component="dd" variant="caption">{record.source === 'prometheus' ? 'Prometheus and live Kubernetes inventory' : 'Caller-supplied samples'}</Typography>
                <Typography component="dt" variant="caption">Record ID</Typography><Typography component="dd" variant="caption" sx={{ fontFamily: 'monospace' }}>{record.id}</Typography>
              </Box>
            </Box>

            <Divider />
            {record.reviewStatus === 'pending' ? <ReviewPanel record={record} /> : (
              <Stack gap={1.5}>
                <Typography variant="body2">
                  {record.reviewStatus === 'approved' ? 'Approved' : 'Rejected'} by <strong>{record.reviewedBy}</strong>{record.reviewedAt ? ` on ${formatDate(record.reviewedAt)}` : ''}.
                </Typography>
                {record.reviewComment && <Typography variant="body2" color="text.secondary">“{record.reviewComment}”</Typography>}
                {record.reviewStatus === 'approved' && <ApplyCommand record={record} />}
              </Stack>
            )}
          </Stack>
        )}
      </Box>
    </Drawer>
  )
}
