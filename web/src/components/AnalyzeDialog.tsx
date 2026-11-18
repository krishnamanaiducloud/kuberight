import { useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import {
  Alert, Box, Button, Chip, Dialog, DialogActions, DialogContent, DialogTitle, LinearProgress, MenuItem, Stack,
  Table, TableBody, TableCell, TableContainer, TableHead, TableRow, TextField, ToggleButton, ToggleButtonGroup, Typography,
} from '@mui/material'
import { api, ApiError, newIdempotencyKey, type CollectionEnvelope, type RecommendationRecord, type Subject, type WorkloadRecord } from '../api'
import { formatPercent } from '../format'

const concurrency = 3

type Target = Subject & { state: 'queued' | 'running' | 'done' | 'failed'; message?: string; record?: RecommendationRecord }

type AnalyzeDialogProps = {
  open: boolean
  onClose: () => void
  /** Analyze every container in this namespace... */
  namespace?: string
  /** ...or only the containers of this workload. */
  workload?: WorkloadRecord
}

function targetsOf(workloads: WorkloadRecord[]): Target[] {
  return workloads.flatMap((workload) => (workload.containers ?? []).map((container) => ({
    namespace: workload.namespace, kind: workload.kind, workload: workload.name, container: container.name, state: 'queued' as const,
  })))
}

function changeLabel(current: number, next: number) {
  return current > 0 ? formatPercent(((next - current) / current) * 100) : 'new'
}

/**
 * Generates and stores recommendations for many containers. Requests run a
 * few at a time from the browser, so each one keeps the caller's own
 * authorization and the API's normal request deadline.
 */
export function AnalyzeDialog({ open, onClose, namespace, workload }: AnalyzeDialogProps) {
  const client = useQueryClient()
  const [profile, setProfile] = useState('balanced')
  const [usageWindow, setUsageWindow] = useState('168h')
  const [targets, setTargets] = useState<Target[]>([])
  const [running, setRunning] = useState(false)
  const [loadError, setLoadError] = useState<string>()
  const abort = useRef<AbortController>(undefined)

  const update = (index: number, patch: Partial<Target>) => setTargets((current) => current.map((item, position) => position === index ? { ...item, ...patch } : item))

  const start = async () => {
    setLoadError(undefined)
    let list: Target[]
    try {
      if (workload) {
        list = targetsOf([workload])
      } else {
        const envelope = await api.get<CollectionEnvelope>(`/api/v1/workloads?page=1&pageSize=200&namespace=${encodeURIComponent(namespace ?? '')}`)
        list = targetsOf(envelope.items as WorkloadRecord[])
      }
    } catch (error) {
      setLoadError(error instanceof ApiError ? error.message : 'Workloads could not be loaded.')
      return
    }
    setTargets(list)
    setRunning(true)
    const controller = new AbortController()
    abort.current = controller
    let next = 0
    const worker = async () => {
      while (!controller.signal.aborted && next < list.length) {
        const index = next++
        const target = list[index]
        update(index, { state: 'running' })
        try {
          const record = await api.post<RecommendationRecord>('/api/v1/recommendations:generate', {
            namespace: target.namespace, kind: target.kind, workload: target.workload, container: target.container, window: usageWindow, profile,
          }, newIdempotencyKey(), controller.signal)
          update(index, { state: 'done', record })
        } catch (error) {
          if (controller.signal.aborted) {
            update(index, { state: 'queued' })
            return
          }
          update(index, { state: 'failed', message: error instanceof ApiError ? error.message : 'Request failed' })
        }
      }
    }
    await Promise.all(Array.from({ length: concurrency }, worker))
    setRunning(false)
    void client.invalidateQueries({ queryKey: ['resource'] })
    void client.invalidateQueries({ queryKey: ['overview'] })
    void client.invalidateQueries({ queryKey: ['workload-recommendations'] })
  }

  const finished = targets.filter((item) => item.state === 'done' || item.state === 'failed').length
  const created = targets.filter((item) => item.state === 'done').length
  const failed = targets.filter((item) => item.state === 'failed').length
  const scope = workload ? `${workload.namespace} / ${workload.kind} / ${workload.name}` : `namespace ${namespace}`
  const close = () => { abort.current?.abort(); setTargets([]); onClose() }

  return (
    <Dialog open={open} onClose={running ? undefined : close} fullWidth maxWidth="md">
      <DialogTitle>Analyze {scope}</DialogTitle>
      <DialogContent dividers>
        {targets.length === 0 ? (
          <Stack gap={2}>
            <Typography color="text.secondary">
              Generates and stores a recommendation for every container in {scope}, using live requests and Prometheus history. Each recommendation then waits for review; nothing is applied.
            </Typography>
            <Stack direction={{ xs: 'column', sm: 'row' }} gap={1.5}>
              <ToggleButtonGroup exclusive size="small" value={profile} onChange={(_, next) => next && setProfile(next)} aria-label="Profile">
                {['conservative', 'balanced', 'aggressive'].map((name) => <ToggleButton key={name} value={name} sx={{ textTransform: 'capitalize' }}>{name}</ToggleButton>)}
              </ToggleButtonGroup>
              <TextField select size="small" label="Usage window" value={usageWindow} onChange={(event) => setUsageWindow(event.target.value)} sx={{ minWidth: 180 }}>
                <MenuItem value="72h">Last 3 days</MenuItem>
                <MenuItem value="168h">Last 7 days</MenuItem>
                <MenuItem value="336h">Last 14 days</MenuItem>
                <MenuItem value="720h">Last 30 days</MenuItem>
              </TextField>
            </Stack>
            {loadError && <Alert severity="error">{loadError}</Alert>}
          </Stack>
        ) : (
          <Stack gap={1.5}>
            <Stack direction="row" justifyContent="space-between" alignItems="center">
              <Typography variant="body2">{finished} of {targets.length} containers analyzed</Typography>
              <Stack direction="row" gap={1}>
                <Chip size="small" color="success" variant="outlined" label={`${created} created`} />
                {failed > 0 && <Chip size="small" color="warning" variant="outlined" label={`${failed} skipped`} />}
              </Stack>
            </Stack>
            <LinearProgress variant="determinate" value={targets.length ? (finished / targets.length) * 100 : 0} aria-label="Analysis progress" />
            <TableContainer sx={{ maxHeight: 360 }}>
              <Table size="small" stickyHeader aria-label="Analysis results">
                <TableHead><TableRow><TableCell>Container</TableCell><TableCell>Result</TableCell><TableCell align="right">CPU</TableCell><TableCell align="right">Memory</TableCell></TableRow></TableHead>
                <TableBody>
                  {targets.map((target) => (
                    <TableRow key={`${target.workload}/${target.container}`}>
                      <TableCell sx={{ maxWidth: 260 }}><Typography variant="body2" noWrap>{target.workload} / {target.container}</Typography></TableCell>
                      <TableCell sx={{ maxWidth: 280 }}>
                        {target.state === 'failed'
                          ? <Typography variant="body2" color="warning.main">{target.message}</Typography>
                          : <Chip size="small" variant="outlined" label={target.state === 'done' ? `Risk: ${target.record?.risk}` : target.state} color={target.state === 'done' ? 'success' : 'default'} />}
                      </TableCell>
                      <TableCell align="right">{target.record ? changeLabel(target.record.currentCpuRequestMilli, target.record.cpuRequestMilli) : '—'}</TableCell>
                      <TableCell align="right">{target.record ? changeLabel(target.record.currentMemoryRequestBytes, target.record.memoryRequestBytes) : '—'}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          </Stack>
        )}
        {targets.length === 0 && workload && (workload.containers ?? []).length === 0 && <Box sx={{ mt: 2 }}><Alert severity="info">This workload reports no containers.</Alert></Box>}
      </DialogContent>
      <DialogActions>
        {running
          ? <Button color="warning" onClick={() => abort.current?.abort()}>Stop</Button>
          : <Button onClick={close}>{targets.length ? 'Close' : 'Cancel'}</Button>}
        {targets.length === 0 && <Button variant="contained" onClick={() => void start()}>Analyze</Button>}
      </DialogActions>
    </Dialog>
  )
}
