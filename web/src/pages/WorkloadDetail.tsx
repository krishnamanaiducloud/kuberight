import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link as RouterLink, useParams, useSearchParams } from 'react-router-dom'
import {
  Alert, Box, Button, Card, CardContent, Chip, MenuItem, Skeleton, Stack, Tab, Table, TableBody, TableCell,
  TableContainer, TableHead, TableRow, Tabs, TextField, Typography,
} from '@mui/material'
import ArrowBackIcon from '@mui/icons-material/ArrowBack'
import AutoFixHighOutlined from '@mui/icons-material/AutoFixHighOutlined'
import PlaylistPlayOutlined from '@mui/icons-material/PlaylistPlayOutlined'
import { api, type ApiRecord, type CollectionEnvelope, type RecommendationRecord, type Subject, type WorkloadRecord } from '../api'
import { AnalyzeDialog } from '../components/AnalyzeDialog'
import { GenerateDialog } from '../components/GenerateDialog'
import { RecommendationDrawer } from '../components/RecommendationDrawer'
import { EvidenceStatusChip } from '../components/RecommendationEvidence'
import { RequestAlert } from '../components/RequestAlert'
import { SignalsPanel } from '../components/SignalsPanel'
import { formatBytes, formatDate, formatMillicores, formatPercent } from '../format'
import { usageWindows } from '../usageWindows'

type WorkloadDetailRecord = WorkloadRecord & { desiredReplicas?: number; readyReplicas?: number; health?: string; cluster?: string }

const reviewColor = { pending: 'warning', approved: 'success', rejected: 'default' } as const
const riskColor = { low: 'success', medium: 'warning', high: 'error' } as const

function delta(current: number, next: number) {
  return current > 0 ? formatPercent(((next - current) / current) * 100) : 'new'
}

/** Everything about one workload: containers, latest advice, usage, history. */
export function WorkloadDetail() {
  const { namespace = '', kind = '', name = '' } = useParams()
  const [searchParams, setSearchParams] = useSearchParams()
  const requestedWindow = searchParams.get('window') ?? '24h'
  const usageWindow = usageWindows.some((option) => option.value === requestedWindow) ? requestedWindow : '24h'
  const container = searchParams.get('container') ?? undefined
  const [generateFor, setGenerateFor] = useState<Subject>()
  const [analyzeOpen, setAnalyzeOpen] = useState(false)
  const [selected, setSelected] = useState<string>()

  const workloadQuery = useQuery({
    queryKey: ['resource', 'workload', namespace, kind, name],
    queryFn: ({ signal }) => api.get<CollectionEnvelope>(`/api/v1/workloads?page=1&pageSize=200&namespace=${encodeURIComponent(namespace)}&filter=${encodeURIComponent(name)}`, { signal }),
  })
  const workload = (workloadQuery.data?.items as WorkloadDetailRecord[] | undefined)?.find((item) => item.kind === kind && item.name === name)
  const autoscalers = useQuery({
    queryKey: ['resource', 'workload-autoscalers', namespace],
    queryFn: ({ signal }) => api.get<CollectionEnvelope>(`/api/v1/autoscalers?page=1&pageSize=200&namespace=${encodeURIComponent(namespace)}`, { signal }),
  })
  const relatedAutoscalers = (autoscalers.data?.items ?? []).filter((item: ApiRecord) => item.targetName === name && (item.targetKind ?? 'Deployment') === kind)
  const history = useQuery({
    queryKey: ['workload-recommendations', namespace, kind, name],
    queryFn: ({ signal }) => api.get<{ items: RecommendationRecord[] }>(`/api/v1/recommendations?page=1&pageSize=200&namespace=${encodeURIComponent(namespace)}&workload=${encodeURIComponent(name)}`, { signal }),
  })
  const recommendations = useMemo(() => (history.data?.items ?? []).filter((item) => !item.kind || item.kind === kind), [history.data, kind])
  const latest = useMemo(() => {
    const byContainer = new Map<string, RecommendationRecord>()
    for (const item of recommendations) if (!byContainer.has(item.container)) byContainer.set(item.container, item)
    return byContainer
  }, [recommendations])

  const containers = workload?.containers ?? []
  const activeContainer = containers.some((item) => item.name === container) ? container : containers[0]?.name
  const subject = (containerName: string): Subject => ({ namespace, kind, workload: name, container: containerName })
  const updateView = (key: 'container' | 'window', value: string) => {
    setSearchParams((current) => {
      const next = new URLSearchParams(current)
      next.set(key, value)
      return next
    }, { replace: true })
  }

  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <Button component={RouterLink} to={`/workloads?namespace=${encodeURIComponent(namespace)}`} startIcon={<ArrowBackIcon />} size="small" sx={{ mb: 1 }}>Workloads</Button>
      <Stack direction={{ xs: 'column', md: 'row' }} justifyContent="space-between" gap={2} sx={{ mb: 3 }}>
        <Box sx={{ minWidth: 0 }}>
          <Typography variant="overline" color="primary.main">{kind.toUpperCase()}</Typography>
          <Typography variant="h3" component="h1" sx={{ fontSize: { xs: '2.125rem', sm: '3rem' }, overflowWrap: 'anywhere' }}>{name}</Typography>
          <Stack direction="row" gap={1} flexWrap="wrap" alignItems="center" sx={{ mt: 1 }}>
            <Typography color="text.secondary">{namespace}</Typography>
            {workload && <Chip size="small" variant="outlined" label={`${workload.readyReplicas ?? 0}/${workload.desiredReplicas ?? 0} ready`} color={workload.health === 'healthy' ? 'success' : 'warning'} />}
            {relatedAutoscalers.map((item) => (
              <Chip key={String(item.id)} size="small" variant="outlined" color="warning" label={`${String(item.kind).replace('HorizontalPodAutoscaler', 'HPA').replace('VerticalPodAutoscaler', 'VPA').replace('KEDAScaledObject', 'KEDA')}${item.kind === 'HorizontalPodAutoscaler' ? ` ${item.minReplicas}–${item.maxReplicas}` : ''}`} />
            ))}
          </Stack>
        </Box>
        <Stack direction="row" gap={1} alignItems="flex-start" flexWrap="wrap">
          <Button variant="contained" startIcon={<PlaylistPlayOutlined />} disabled={!workload || containers.length === 0} onClick={() => setAnalyzeOpen(true)}>Analyze all containers</Button>
        </Stack>
      </Stack>

      {workloadQuery.error && <Box sx={{ mb: 2 }}><RequestAlert error={workloadQuery.error} resource="this workload" namespaceScoped onRetry={() => void workloadQuery.refetch()} /></Box>}
      {autoscalers.error && <Box sx={{ mb: 2 }}><RequestAlert error={autoscalers.error} resource="autoscaler context" namespaceScoped onRetry={() => void autoscalers.refetch()} /></Box>}
      {history.error && <Box sx={{ mb: 2 }}><RequestAlert error={history.error} resource="recommendation history" namespaceScoped onRetry={() => void history.refetch()} /></Box>}
      {workloadQuery.data && !workload && <Alert severity="warning" sx={{ mb: 2 }}>This workload was not found in the inventory. It may have been deleted, or it is outside the configured scope.</Alert>}

      <Card sx={{ mb: 2 }}>
        <CardContent>
          <Typography variant="h6" sx={{ mb: 1.5 }}>Containers</Typography>
          {workloadQuery.isLoading ? <Skeleton variant="rounded" height={96} /> : (
            <TableContainer>
              <Table size="small" aria-label="Containers and latest recommendations">
                <TableHead>
                  <TableRow>
                    <TableCell sx={{ fontWeight: 800 }}>Container</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">CPU request</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">CPU limit</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">Memory request</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">Memory limit</TableCell>
                    <TableCell sx={{ fontWeight: 800 }}>Latest recommendation</TableCell>
                    <TableCell />
                  </TableRow>
                </TableHead>
                <TableBody>
                  {containers.map((item) => {
                    const advice = latest.get(item.name)
                    return (
                      <TableRow key={item.name} hover>
                        <TableCell sx={{ fontWeight: 700 }}>{item.name}</TableCell>
                        <TableCell align="right">{item.cpuRequestMilli ? formatMillicores(item.cpuRequestMilli) : 'unset'}</TableCell>
                        <TableCell align="right">{item.cpuLimitMilli ? formatMillicores(item.cpuLimitMilli) : 'unset'}</TableCell>
                        <TableCell align="right">{item.memoryRequestBytes ? formatBytes(item.memoryRequestBytes) : 'unset'}</TableCell>
                        <TableCell align="right">{item.memoryLimitBytes ? formatBytes(item.memoryLimitBytes) : 'unset'}</TableCell>
                        <TableCell>
                          {advice ? (
                            <Stack direction="row" gap={1} alignItems="center" flexWrap="wrap">
                              <Typography variant="body2">CPU {formatMillicores(advice.cpuRequestMilli)} ({delta(advice.currentCpuRequestMilli, advice.cpuRequestMilli)})</Typography>
                              <Typography variant="body2">· Memory {formatBytes(advice.memoryRequestBytes)} ({delta(advice.currentMemoryRequestBytes, advice.memoryRequestBytes)})</Typography>
                              <EvidenceStatusChip record={advice} />
                              <Chip size="small" variant="outlined" label={`${advice.risk} risk`} color={riskColor[advice.risk]} />
                              <Chip size="small" variant="outlined" label={advice.reviewStatus} color={reviewColor[advice.reviewStatus]} />
                            </Stack>
                          ) : <Typography variant="body2" color="text.secondary">None yet</Typography>}
                        </TableCell>
                        <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
                          {advice && <Button size="small" onClick={() => setSelected(advice.id)}>Details</Button>}
                          <Button size="small" startIcon={<AutoFixHighOutlined />} onClick={() => setGenerateFor(subject(item.name))}>Generate</Button>
                        </TableCell>
                      </TableRow>
                    )
                  })}
                </TableBody>
              </Table>
            </TableContainer>
          )}
        </CardContent>
      </Card>

      {activeContainer && (
        <Box sx={{ mb: 2 }}>
          <Stack direction={{ xs: 'column', sm: 'row' }} justifyContent="space-between" alignItems={{ sm: 'center' }} gap={1} sx={{ mb: 1.5 }}>
            <Tabs value={activeContainer} onChange={(_, next) => updateView('container', next)} variant="scrollable" allowScrollButtonsMobile aria-label="Container usage">
              {containers.map((item) => <Tab key={item.name} value={item.name} label={item.name} />)}
            </Tabs>
            <TextField select size="small" label="Window" value={usageWindow} onChange={(event) => updateView('window', event.target.value)} sx={{ minWidth: 140 }}>
              {usageWindows.map((option) => <MenuItem key={option.value} value={option.value}>{option.label}</MenuItem>)}
            </TextField>
          </Stack>
          <SignalsPanel subject={subject(activeContainer)} usageWindow={usageWindow} />
        </Box>
      )}

      <Card>
        <CardContent>
          <Typography variant="h6" sx={{ mb: 1.5 }}>Recommendation history</Typography>
          {recommendations.length === 0 ? <Typography color="text.secondary">No recommendations have been generated for this workload.</Typography> : (
            <TableContainer>
              <Table size="small" aria-label="Recommendation history">
                <TableHead>
                  <TableRow>
                    <TableCell sx={{ fontWeight: 800 }}>Created</TableCell>
                    <TableCell sx={{ fontWeight: 800 }}>Container</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">CPU request</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">Memory request</TableCell>
                    <TableCell sx={{ fontWeight: 800 }}>Risk</TableCell>
                    <TableCell sx={{ fontWeight: 800 }}>Evidence</TableCell>
                    <TableCell sx={{ fontWeight: 800 }}>Review</TableCell>
                  </TableRow>
                </TableHead>
                <TableBody>
                  {recommendations.map((item) => (
                    <TableRow key={item.id} hover tabIndex={0} sx={{ cursor: 'pointer' }} onClick={() => setSelected(item.id)} onKeyDown={(event) => { if (event.key === 'Enter') setSelected(item.id) }}>
                      <TableCell>{formatDate(item.createdAt)}</TableCell>
                      <TableCell>{item.container}</TableCell>
                      <TableCell align="right">{formatMillicores(item.cpuRequestMilli)} ({delta(item.currentCpuRequestMilli, item.cpuRequestMilli)})</TableCell>
                      <TableCell align="right">{formatBytes(item.memoryRequestBytes)} ({delta(item.currentMemoryRequestBytes, item.memoryRequestBytes)})</TableCell>
                      <TableCell><Chip size="small" variant="outlined" label={item.risk} color={riskColor[item.risk]} /></TableCell>
                      <TableCell><EvidenceStatusChip record={item} /></TableCell>
                      <TableCell><Chip size="small" variant="outlined" label={item.reviewStatus} color={reviewColor[item.reviewStatus]} /></TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          )}
        </CardContent>
      </Card>

      {generateFor && (
        <GenerateDialog key={generateFor.container} open initialSubject={generateFor} onClose={() => setGenerateFor(undefined)} onCreated={(record) => { setGenerateFor(undefined); setSelected(record.id) }} />
      )}
      {workload && <AnalyzeDialog open={analyzeOpen} workload={workload} onClose={() => setAnalyzeOpen(false)} />}
      <RecommendationDrawer id={selected} onClose={() => setSelected(undefined)} />
    </Box>
  )
}
