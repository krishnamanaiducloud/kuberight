import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert, Box, Button, Chip, CircularProgress, Dialog, DialogActions, DialogContent, DialogTitle, Divider,
  LinearProgress, MenuItem, Stack, TextField, Typography, useMediaQuery,
} from '@mui/material'
import { useTheme } from '@mui/material/styles'
import { api, ApiError, newIdempotencyKey, type Policy, type PolicyProfile, type RecommendationPreview, type RecommendationRecord, type Subject } from '../api'
import { formatBytes, formatMillicores } from '../format'
import { useDebounced } from '../hooks'
import { PolicyTuner } from './PolicyTuner'
import { ResourceTable } from './ResourceTable'
import { BacktestTable, TrendChips } from './Backtest'
import { WorkloadPicker } from './WorkloadPicker'

const windows = [
  { value: '24h', label: 'Last 24 hours' },
  { value: '72h', label: 'Last 3 days' },
  { value: '168h', label: 'Last 7 days' },
  { value: '336h', label: 'Last 14 days' },
  { value: '720h', label: 'Last 30 days' },
]

const riskColor = { low: 'success', medium: 'warning', high: 'error' } as const

type GenerateDialogProps = {
  open: boolean
  onClose: () => void
  onCreated: (record: RecommendationRecord) => void
  /** Preselects a container, for example from a workload page. */
  initialSubject?: Subject
}

function PreviewPanel({ preview }: { preview: RecommendationPreview }) {
  const { recommendation, events } = preview
  const throttling = events.throttlingAvailable ? events.throttlingRatio : undefined
  return (
    <Stack gap={1.5}>
      <Stack direction="row" gap={1} flexWrap="wrap">
        <Chip size="small" variant="outlined" color={riskColor[recommendation.risk]} label={`Risk: ${recommendation.risk}`} />
        <Chip size="small" variant="outlined" label={`Confidence: ${recommendation.confidence} (${Math.round(recommendation.confidenceScore * 100)}%)`} />
        {throttling !== undefined && (
          <Chip size="small" variant="outlined" color={throttling >= 0.05 ? 'warning' : 'success'} label={`CPU throttled ${(throttling * 100).toFixed(1)}%`} />
        )}
        {events.available && events.oomKills > 0 && <Chip size="small" variant="outlined" color="error" label={`${events.oomKills} OOM kill${events.oomKills === 1 ? '' : 's'}`} />}
      </Stack>
      <ResourceTable record={recommendation} />
      <TrendChips trends={recommendation.trends} />
      {recommendation.backtest && <BacktestTable backtest={recommendation.backtest} />}
      <Typography variant="caption" color="text.secondary">
        {recommendation.reason}. Observed P95 CPU {formatMillicores(recommendation.cpuStatisticsMilli.p95)}, peak memory {formatBytes(recommendation.memoryStatisticsBytes.max)},
        from {recommendation.dataQuality.cpuSampleCount} samples over {preview.window.replace(/0m0s$/, '').replace(/h$/, ' h')}.
      </Typography>
      {(recommendation.dataQuality.issues ?? []).map((issue) => <Alert key={issue} severity="warning" variant="outlined">{issue}</Alert>)}
      {(recommendation.warnings ?? []).length > 0 && (
        <Box component="ul" sx={{ m: 0, pl: 2.5, color: 'text.secondary', fontSize: 13 }}>
          {(recommendation.warnings ?? []).map((warning) => <li key={warning}>{warning}</li>)}
        </Box>
      )}
    </Stack>
  )
}

/**
 * Generates a recommendation server-side from live inventory and Prometheus.
 * Every change to the inputs refreshes a dry-run preview, so the policy can be
 * tuned against real usage before anything is stored.
 */
export function GenerateDialog({ open, onClose, onCreated, initialSubject }: GenerateDialogProps) {
  const client = useQueryClient()
  const theme = useTheme()
  const fullScreen = useMediaQuery(theme.breakpoints.down('sm'))
  const [subject, setSubject] = useState<Subject | undefined>(initialSubject)
  const [usageWindow, setUsageWindow] = useState('168h')
  const [criticality, setCriticality] = useState('')
  const [profile, setProfile] = useState('balanced')
  const [policy, setPolicy] = useState<Policy>()
  const [customized, setCustomized] = useState(false)
  const [idempotencyKey, setIdempotencyKey] = useState(newIdempotencyKey)


  const profiles = useQuery({
    queryKey: ['policies'],
    queryFn: ({ signal }) => api.get<{ items: PolicyProfile[] }>('/api/v1/policies', { signal }),
    staleTime: Infinity,
  })
  const activePolicy = policy ?? profiles.data?.items.find((item) => item.name === profile)?.policy
  const body = subject && {
    ...subject, window: usageWindow, profile,
    ...(criticality ? { criticality } : {}),
    ...(customized && activePolicy ? { policy: activePolicy } : {}),
  }
  const debouncedBody = useDebounced(body, 400)
  const preview = useQuery({
    queryKey: ['generate-preview', debouncedBody],
    enabled: open && Boolean(debouncedBody),
    queryFn: ({ signal }) => api.post<RecommendationPreview>('/api/v1/recommendations:generate', { ...debouncedBody, dryRun: true }, undefined, signal),
    retry: false,
    placeholderData: (previous) => previous,
  })
  const save = useMutation({
    mutationFn: () => api.post<RecommendationRecord>('/api/v1/recommendations:generate', body, idempotencyKey),
    onSuccess: (record) => {
      void client.invalidateQueries({ queryKey: ['resource'] })
      void client.invalidateQueries({ queryKey: ['overview'] })
      void client.invalidateQueries({ queryKey: ['workload-recommendations'] })
      setIdempotencyKey(newIdempotencyKey())
      onCreated(record)
    },
  })
  // Any input change is a different request, so it needs a new key.
  const changed = () => { setIdempotencyKey(newIdempotencyKey()); save.reset() }
  const errorText = (error: unknown, fallback: string) => error instanceof ApiError ? error.message : error ? fallback : undefined
  const previewError = errorText(preview.error, 'The preview could not be calculated.')
  const saveError = errorText(save.error, 'The recommendation could not be saved.')

  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="lg" fullScreen={fullScreen}>
      <DialogTitle>Generate a recommendation</DialogTitle>
      <DialogContent dividers>
        <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', md: '5fr 6fr' }, gap: 3 }}>
          <Stack gap={2}>
            <Typography variant="body2" color="text.secondary">
              KubeRight reads the container's current requests, limits, replicas, and autoscalers from Kubernetes, and its CPU, memory, throttling, restart, and OOM history from Prometheus.
            </Typography>
            {initialSubject
              ? <Typography sx={{ fontWeight: 700 }}>{initialSubject.namespace} / {initialSubject.kind} / {initialSubject.workload} · {initialSubject.container}</Typography>
              : <WorkloadPicker value={subject} onChange={(next) => { setSubject(next); changed() }} />}
            <Stack direction={{ xs: 'column', sm: 'row' }} gap={1.5}>
              <TextField select size="small" label="Usage window" value={usageWindow} onChange={(event) => { setUsageWindow(event.target.value); changed() }} sx={{ flex: 1 }}>
                {windows.map((option) => <MenuItem key={option.value} value={option.value}>{option.label}</MenuItem>)}
              </TextField>
              <TextField select size="small" label="Criticality" value={criticality} onChange={(event) => { setCriticality(event.target.value); changed() }} sx={{ flex: 1 }} helperText="High limits reductions to 10%">
                <MenuItem value="">Not specified</MenuItem>
                <MenuItem value="low">Low</MenuItem>
                <MenuItem value="medium">Medium</MenuItem>
                <MenuItem value="high">High</MenuItem>
              </TextField>
            </Stack>
            {profiles.data && activePolicy && (
              <PolicyTuner
                profiles={profiles.data.items}
                profile={profile}
                policy={activePolicy}
                customized={customized}
                onChange={(nextProfile, nextPolicy, isCustom) => { setProfile(nextProfile); setPolicy(nextPolicy); setCustomized(isCustom); changed() }}
              />
            )}
          </Stack>
          <Box>
            <Stack direction="row" alignItems="center" justifyContent="space-between" sx={{ mb: 1 }}>
              <Typography variant="h6">Live preview</Typography>
              {preview.data && <Typography variant="caption" color="text.secondary">Not saved until you choose Save</Typography>}
            </Stack>
            <Divider sx={{ mb: 1.5 }} />
            {preview.isFetching && <LinearProgress sx={{ mb: 1.5 }} aria-label="Calculating preview" />}
            {!subject && <Typography color="text.secondary">Choose a workload container to see its recommendation.</Typography>}
            {subject && previewError && <Alert severity="error">{previewError}</Alert>}
            {subject && !previewError && preview.data && <Box sx={{ opacity: preview.isFetching ? 0.6 : 1, transition: 'opacity .2s' }}><PreviewPanel preview={preview.data} /></Box>}
          </Box>
        </Box>
        {saveError && <Alert severity="error" sx={{ mt: 2 }}>{saveError}</Alert>}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" disabled={!subject || save.isPending || Boolean(previewError)} onClick={() => save.mutate()} startIcon={save.isPending ? <CircularProgress size={16} /> : undefined}>
          Save recommendation
        </Button>
      </DialogActions>
    </Dialog>
  )
}
