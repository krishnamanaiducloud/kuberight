import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert, Box, Button, Card, CardContent, Chip, Dialog, DialogActions, DialogContent,
  DialogTitle, LinearProgress, MenuItem, Skeleton, Stack, Table, TableBody, TableCell,
  TableContainer, TableHead, TableRow, TextField, Typography,
} from '@mui/material'
import AddTaskOutlined from '@mui/icons-material/AddTaskOutlined'
import PauseCircleOutline from '@mui/icons-material/PauseCircleOutline'
import PlayCircleOutline from '@mui/icons-material/PlayCircleOutline'
import RefreshIcon from '@mui/icons-material/Refresh'
import { api, newIdempotencyKey, type ApiStatus } from '../api'
import { RequestAlert } from '../components/RequestAlert'

type FleetSchedule = {
  id: string
  name: string
  enabled: boolean
  intervalSeconds: number
  scope: { cluster?: string; namespaces?: string[]; kinds?: string[]; filter?: string }
  policyId?: string
  windowSeconds: number
  nextRunAt: string
  lastRunAt?: string
  version: number
}

type FleetFailure = {
  cluster?: string
  namespace?: string
  kind?: string
  workload?: string
  container?: string
  stage: string
  error: string
}

type FleetScan = {
  id: string
  scheduleId?: string
  scheduledFor: string
  availableAt?: string
  scope: FleetSchedule['scope']
  policyId?: string
  windowSeconds: number
  status: 'queued' | 'running' | 'completed' | 'failed' | 'canceled'
  expected: number
  processed: number
  succeeded: number
  failed: number
  attempt: number
  maxAttempts: number
  lastError?: string
  failureDetails?: FleetFailure[]
  completedAt?: string
}

type FleetCollection<T> = {
  items: T[]
  pagination: { page: number; pageSize: number; total: number }
  status: ApiStatus
}

const dateTime = (value?: string) => value ? new Date(value).toLocaleString() : '—'
const duration = (seconds: number) => {
  if (seconds % 86_400 === 0) return `${seconds / 86_400}d`
  if (seconds % 3_600 === 0) return `${seconds / 3_600}h`
  return `${Math.round(seconds / 60)}m`
}

function statusColor(status: FleetScan['status']): 'default' | 'info' | 'success' | 'warning' | 'error' {
  if (status === 'completed') return 'success'
  if (status === 'running') return 'info'
  if (status === 'queued') return 'warning'
  if (status === 'failed') return 'error'
  return 'default'
}

function scopeLabel(scope: FleetSchedule['scope']) {
  const namespace = scope.namespaces?.length ? scope.namespaces.join(', ') : 'all visible namespaces'
  const kinds = scope.kinds?.length ? ` · ${scope.kinds.join(', ')}` : ''
  return `${namespace}${kinds}`
}

function MetricCard({ label, value, detail, loading }: { label: string; value: number; detail: string; loading: boolean }) {
  return (
    <Card>
      <CardContent>
        <Typography color="text.secondary" variant="body2">{label}</Typography>
        {loading ? <Skeleton width={80} height={54} /> : <Typography variant="h3" sx={{ mt: .5 }}>{value}</Typography>}
        <Typography variant="caption" color="text.secondary">{detail}</Typography>
      </CardContent>
    </Card>
  )
}

function ScanDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [namespace, setNamespace] = useState('')
  const [windowDays, setWindowDays] = useState(7)
  const [maxAttempts, setMaxAttempts] = useState(3)
  const mutation = useMutation({
    mutationFn: () => api.post<FleetScan>('/api/v1/fleet/scans', {
      scope: namespace ? { namespaces: [namespace] } : {},
      windowSeconds: windowDays * 86_400,
      maxAttempts,
    }, newIdempotencyKey()),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['fleet-scans'] })
      onClose()
    },
  })
  const validNamespace = namespace === '' || /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(namespace)
  return (
    <Dialog open={open} onClose={mutation.isPending ? undefined : onClose} fullWidth maxWidth="sm">
      <DialogTitle>Queue an evidence scan</DialogTitle>
      <DialogContent>
        <Stack gap={2} sx={{ pt: 1 }}>
          <Alert severity="info">The scan reads Kubernetes and Prometheus evidence and creates recommendations. It never changes workloads.</Alert>
          {mutation.error && <RequestAlert error={mutation.error} resource="fleet scan" />}
          <TextField
            label="Namespace (optional)"
            value={namespace}
            onChange={(event) => setNamespace(event.target.value.trim().toLowerCase())}
            error={!validNamespace}
            helperText={validNamespace ? 'Leave empty only when you intend to scan every namespace your identity can access.' : 'Use a valid Kubernetes namespace.'}
            inputProps={{ maxLength: 63 }}
          />
          <TextField select label="Evidence window" value={windowDays} onChange={(event) => setWindowDays(Number(event.target.value))}>
            {[1, 3, 7, 14, 30].map((days) => <MenuItem key={days} value={days}>{days} day{days === 1 ? '' : 's'}</MenuItem>)}
          </TextField>
          <TextField select label="Maximum attempts" value={maxAttempts} onChange={(event) => setMaxAttempts(Number(event.target.value))}>
            {[1, 2, 3, 4, 5].map((attempts) => <MenuItem key={attempts} value={attempts}>{attempts}</MenuItem>)}
          </TextField>
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={mutation.isPending}>Cancel</Button>
        <Button variant="contained" onClick={() => mutation.mutate()} disabled={!validNamespace || mutation.isPending} startIcon={<AddTaskOutlined />}>
          {mutation.isPending ? 'Queueing…' : 'Queue scan'}
        </Button>
      </DialogActions>
    </Dialog>
  )
}

export function Fleet({ title, description }: { title: string; description: string }) {
  const queryClient = useQueryClient()
  const [dialogOpen, setDialogOpen] = useState(false)
  const schedules = useQuery({
    queryKey: ['fleet-schedules'],
    queryFn: ({ signal }) => api.get<FleetCollection<FleetSchedule>>('/api/v1/fleet/schedules?page=1&pageSize=100', { signal }),
  })
  const scans = useQuery({
    queryKey: ['fleet-scans'],
    queryFn: ({ signal }) => api.get<FleetCollection<FleetScan>>('/api/v1/fleet/scans?page=1&pageSize=100', { signal }),
    refetchInterval: (query) => query.state.data?.items.some((scan) => scan.status === 'queued' || scan.status === 'running') ? 5_000 : false,
  })
  const changeState = useMutation({
    mutationFn: ({ id, action }: { id: string; action: 'cancel' | 'resume' }) => api.post<FleetScan>(`/api/v1/fleet/scans/${encodeURIComponent(id)}/${action}`, {}),
    onSuccess: async () => queryClient.invalidateQueries({ queryKey: ['fleet-scans'] }),
  })
  const items = scans.data?.items ?? []
  const counts = useMemo(() => ({
    active: items.filter((scan) => ['queued', 'running'].includes(scan.status)).length,
    completed: items.filter((scan) => scan.status === 'completed').length,
    failed: items.filter((scan) => scan.status === 'failed').length,
    schedules: schedules.data?.items.filter((schedule) => schedule.enabled).length ?? 0,
  }), [items, schedules.data?.items])
  const loading = scans.isLoading || schedules.isLoading
  const refresh = () => {
    void scans.refetch()
    void schedules.refetch()
  }

  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <Stack direction={{ xs: 'column', sm: 'row' }} justifyContent="space-between" alignItems={{ sm: 'flex-end' }} gap={2} sx={{ mb: 3 }}>
        <Box>
          <Typography variant="overline" color="primary.main">CONTINUOUS OPTIMIZATION</Typography>
          <Typography variant="h3" component="h1" sx={{ fontSize: { xs: '2.125rem', sm: '3rem' } }}>{title}</Typography>
          <Typography color="text.secondary" sx={{ mt: 1, maxWidth: 780 }}>{description}</Typography>
        </Box>
        <Stack direction="row" gap={1}>
          <Button startIcon={<RefreshIcon />} onClick={refresh} disabled={scans.isFetching || schedules.isFetching}>Refresh</Button>
          <Button variant="contained" startIcon={<AddTaskOutlined />} onClick={() => setDialogOpen(true)}>New scan</Button>
        </Stack>
      </Stack>

      {(scans.error || schedules.error || changeState.error) && <RequestAlert error={scans.error ?? schedules.error ?? changeState.error} resource="fleet operations" onRetry={refresh} />}
      <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr 1fr', lg: 'repeat(4, 1fr)' }, gap: 2, mb: 3 }}>
        <MetricCard label="Active scans" value={counts.active} detail="Queued or running" loading={loading} />
        <MetricCard label="Completed" value={counts.completed} detail="Within the latest 100 scans" loading={loading} />
        <MetricCard label="Failed" value={counts.failed} detail="After all configured attempts" loading={loading} />
        <MetricCard label="Enabled schedules" value={counts.schedules} detail="Durably coordinated" loading={loading} />
      </Box>

      <Card sx={{ mb: 3 }}>
        <CardContent>
          <Typography variant="h5">Scan history</Typography>
          <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>A fixed evidence window is retained across retries. Progress and bounded target failures are durable.</Typography>
          <TableContainer>
            <Table size="small" aria-label="Fleet scan history">
              <TableHead><TableRow><TableCell>Status</TableCell><TableCell>Scope</TableCell><TableCell>Progress</TableCell><TableCell>Evidence</TableCell><TableCell>Attempts</TableCell><TableCell>Scheduled</TableCell><TableCell align="right">Action</TableCell></TableRow></TableHead>
              <TableBody>
                {scans.isLoading && <TableRow><TableCell colSpan={7}><LinearProgress /></TableCell></TableRow>}
                {!scans.isLoading && items.length === 0 && <TableRow><TableCell colSpan={7}><Typography color="text.secondary">No fleet scans have been queued.</Typography></TableCell></TableRow>}
                {items.map((scan) => {
                  const progress = scan.expected > 0 ? Math.min(100, Math.round(scan.processed / scan.expected * 100)) : 0
                  return (
                    <TableRow key={scan.id} hover>
                      <TableCell><Chip size="small" label={scan.status} color={statusColor(scan.status)} /></TableCell>
                      <TableCell><Typography variant="body2">{scopeLabel(scan.scope)}</Typography><Typography variant="caption" color="text.secondary">{scan.id}</Typography></TableCell>
                      <TableCell sx={{ minWidth: 150 }}><LinearProgress variant="determinate" value={progress} color={scan.failed ? 'warning' : 'primary'} /><Typography variant="caption">{scan.processed}/{scan.expected} · {scan.succeeded} succeeded · {scan.failed} failed</Typography></TableCell>
                      <TableCell><Typography variant="body2">{duration(scan.windowSeconds)}</Typography>{scan.lastError && <Typography variant="caption" color="error.main" title={scan.failureDetails?.map((failure) => `${failure.stage}: ${failure.error}`).join('\n')}>{scan.lastError}</Typography>}</TableCell>
                      <TableCell>{scan.attempt}/{scan.maxAttempts}</TableCell>
                      <TableCell>{dateTime(scan.scheduledFor)}</TableCell>
                      <TableCell align="right">
                        {['queued', 'running'].includes(scan.status) && <Button size="small" color="inherit" startIcon={<PauseCircleOutline />} disabled={changeState.isPending} onClick={() => changeState.mutate({ id: scan.id, action: 'cancel' })}>Cancel</Button>}
                        {['failed', 'canceled'].includes(scan.status) && <Button size="small" startIcon={<PlayCircleOutline />} disabled={changeState.isPending} onClick={() => changeState.mutate({ id: scan.id, action: 'resume' })}>Resume</Button>}
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </TableContainer>
        </CardContent>
      </Card>

      <Card>
        <CardContent>
          <Typography variant="h5">Schedules</Typography>
          <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>Persistent schedules are claimed by one worker at a time, so horizontal API replicas do not duplicate scans.</Typography>
          <TableContainer>
            <Table size="small" aria-label="Fleet scan schedules">
              <TableHead><TableRow><TableCell>Name</TableCell><TableCell>State</TableCell><TableCell>Scope</TableCell><TableCell>Cadence</TableCell><TableCell>Evidence</TableCell><TableCell>Next run</TableCell><TableCell>Last run</TableCell></TableRow></TableHead>
              <TableBody>
                {schedules.isLoading && <TableRow><TableCell colSpan={7}><LinearProgress /></TableCell></TableRow>}
                {!schedules.isLoading && (schedules.data?.items.length ?? 0) === 0 && <TableRow><TableCell colSpan={7}><Typography color="text.secondary">No schedules are configured. Manual scans remain available.</Typography></TableCell></TableRow>}
                {schedules.data?.items.map((schedule) => (
                  <TableRow key={schedule.id} hover>
                    <TableCell><Typography fontWeight={700}>{schedule.name}</Typography><Typography variant="caption" color="text.secondary">v{schedule.version}</Typography></TableCell>
                    <TableCell><Chip size="small" label={schedule.enabled ? 'enabled' : 'paused'} color={schedule.enabled ? 'success' : 'default'} /></TableCell>
                    <TableCell>{scopeLabel(schedule.scope)}</TableCell>
                    <TableCell>{duration(schedule.intervalSeconds)}</TableCell>
                    <TableCell>{duration(schedule.windowSeconds)}</TableCell>
                    <TableCell>{dateTime(schedule.nextRunAt)}</TableCell>
                    <TableCell>{dateTime(schedule.lastRunAt)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableContainer>
        </CardContent>
      </Card>
      <ScanDialog open={dialogOpen} onClose={() => setDialogOpen(false)} />
    </Box>
  )
}
