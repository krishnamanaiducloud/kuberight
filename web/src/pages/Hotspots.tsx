import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link as RouterLink, useSearchParams } from 'react-router-dom'
import {
  Alert, Box, Button, Card, CardContent, Chip, LinearProgress, Link, MenuItem, Skeleton, Stack, Table, TableBody, TableCell,
  TableContainer, TableHead, TableRow, TextField, Typography,
} from '@mui/material'
import AutoFixHighOutlined from '@mui/icons-material/AutoFixHighOutlined'
import ErrorOutline from '@mui/icons-material/ErrorOutline'
import InfoOutlined from '@mui/icons-material/InfoOutlined'
import WarningAmberOutlined from '@mui/icons-material/WarningAmberOutlined'
import { api, type ApiStatus, type HotspotRow, type Subject } from '../api'
import { statusColors } from '../chartColors'
import { GenerateDialog } from '../components/GenerateDialog'
import { RequestAlert } from '../components/RequestAlert'
import { formatMillicores } from '../format'
import { PageHeader } from './PageHeader'

const windows = [
  { value: '24h', label: '24 hours' },
  { value: '72h', label: '3 days' },
  { value: '168h', label: '7 days' },
]

const severity = {
  critical: { label: 'Critical', icon: <ErrorOutline fontSize="small" sx={{ color: statusColors.critical }} /> },
  warning: { label: 'Warning', icon: <WarningAmberOutlined fontSize="small" sx={{ color: statusColors.warning }} /> },
  info: { label: 'Info', icon: <InfoOutlined fontSize="small" color="info" /> },
}

const percent = (value?: number) => (value === undefined ? '—' : `${(value * 100).toFixed(1)}%`)

type HotspotsResponse = { items: HotspotRow[]; status: ApiStatus; summary?: { throttled: number; pinned: number; spiky: number; window: string; podsUnmatched: number } }

function Tile({ label, value, note }: { label: string; value?: number; note: string }) {
  return (
    <Card>
      <CardContent>
        <Typography variant="body2" color="text.secondary">{label}</Typography>
        {value === undefined ? <Skeleton width="40%" height={48} /> : <Typography variant="h4" component="p" sx={{ fontWeight: 750, mt: 1 }}>{value}</Typography>}
        <Typography variant="caption" color="text.secondary">{note}</Typography>
      </CardContent>
    </Card>
  )
}

/** Containers ranked by CPU throttling, capped usage, and spikiness. */
export function Hotspots({ title, description }: { title: string; description: string }) {
  const [searchParams, setSearchParams] = useSearchParams()
  const requestedWindow = searchParams.get('window') ?? '24h'
  const usageWindow = windows.some((option) => option.value === requestedWindow) ? requestedWindow : '24h'
  const namespace = searchParams.get('namespace') ?? ''
  const allNamespaces = searchParams.get('scope') === 'all'
  const namespaceDraft = searchParams.get('namespaceDraft') ?? namespace
  const validNamespace = namespaceDraft === '' || (namespaceDraft.length <= 63 && /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(namespaceDraft))
  const scopeReady = namespace !== '' || allNamespaces
  const [generateFor, setGenerateFor] = useState<Subject>()
  const query = useQuery({
    queryKey: ['resource', 'hotspots', namespace, usageWindow],
    queryFn: ({ signal }) => api.get<HotspotsResponse>(`/api/v1/hotspots?window=${usageWindow}${namespace ? `&namespace=${encodeURIComponent(namespace)}` : ''}`, { signal }),
    enabled: scopeReady,
    retry: false,
  })
  const data = query.data
  const updateScope = (fleetWide = false) => {
    if (!fleetWide && (!namespaceDraft || !validNamespace)) return
    setSearchParams((current) => {
      const next = new URLSearchParams(current)
      if (fleetWide) {
        next.delete('namespace')
        next.delete('namespaceDraft')
        next.set('scope', 'all')
      } else {
        next.set('namespace', namespaceDraft)
        next.delete('namespaceDraft')
        next.delete('scope')
      }
      return next
    })
  }
  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <PageHeader eyebrow="CPU HEALTH" title={title} description={description}>
        <Stack direction={{ xs: 'column', sm: 'row' }} gap={1.5} sx={{ mt: 2 }}>
          <TextField
            size="small"
            label="Namespace"
            value={namespaceDraft}
            onChange={(event) => {
              const value = event.target.value.trim().toLowerCase()
              setSearchParams((current) => {
                const next = new URLSearchParams(current)
                if (value === namespace) next.delete('namespaceDraft')
                else next.set('namespaceDraft', value)
                return next
              }, { replace: true })
            }}
            onKeyDown={(event) => { if (event.key === 'Enter') updateScope() }}
            error={!validNamespace}
            helperText={!validNamespace ? 'Use a DNS-1123 namespace name.' : namespace ? `Scoped to ${namespace}` : allNamespaces ? 'All visible namespaces selected.' : 'Required before querying workloads.'}
            inputProps={{ maxLength: 63 }}
            sx={{ minWidth: 220 }}
          />
          <Button variant="contained" onClick={() => updateScope()} disabled={!namespaceDraft || !validNamespace}>Apply</Button>
          <Button variant="outlined" onClick={() => updateScope(true)}>All visible</Button>
          <TextField select size="small" label="Window" value={usageWindow} onChange={(event) => {
            setSearchParams((current) => {
              const next = new URLSearchParams(current)
              next.set('window', event.target.value)
              return next
            }, { replace: true })
          }} sx={{ minWidth: 160 }}>
            {windows.map((option) => <MenuItem key={option.value} value={option.value}>{option.label}</MenuItem>)}
          </TextField>
        </Stack>
      </PageHeader>
      {query.isFetching && <LinearProgress sx={{ mb: 2 }} />}
      {!scopeReady && <Alert severity="info" sx={{ mb: 2 }}><strong>Choose a namespace to begin.</strong> Select <strong>All visible</strong> only when this identity has fleet-level list access.</Alert>}
      {query.error && <Box sx={{ mb: 2 }}><RequestAlert error={query.error} resource="CPU hotspots" namespaceScoped onRetry={() => void query.refetch()} /></Box>}
      {data?.status.available === false && <Alert severity="info" sx={{ mb: 2 }}>{data.status.reason}</Alert>}
      {data?.status.available !== false && (
        <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', md: 'repeat(3, 1fr)' }, gap: 2, mb: 2 }}>
          <Tile label="Throttled containers" value={data?.summary?.throttled} note="≥5% of periods overall or ≥25% in peak intervals" />
          <Tile label="Pinned at CPU limit" value={data?.summary?.pinned} note="Usage reaches the limit, so true demand is hidden" />
          <Tile label="Spiky containers" value={data?.summary?.spiky} note="Peak usage at least 3× the median" />
        </Box>
      )}
      {data && data.items.length === 0 && data.status.available && <Alert severity="success">No throttled, capped, or spiky containers were found in this window.</Alert>}
      {data && data.items.length > 0 && (
        <Card>
          <CardContent>
            <TableContainer>
              <Table size="small" aria-label="CPU hotspots">
                <TableHead>
                  <TableRow>
                    <TableCell sx={{ fontWeight: 800 }}>Severity</TableCell>
                    <TableCell sx={{ fontWeight: 800 }}>Workload / container</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">Throttled</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">Peak throttled</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">Peak ÷ median</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">Peak CPU</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">Request / limit</TableCell>
                    <TableCell sx={{ fontWeight: 800, minWidth: 260 }}>Findings</TableCell>
                    <TableCell />
                  </TableRow>
                </TableHead>
                <TableBody>
                  {data.items.map((row) => (
                    <TableRow key={`${row.namespace}/${row.kind}/${row.workload}/${row.container}`} hover>
                      <TableCell>
                        <Stack direction="row" gap={0.75} alignItems="center">{severity[row.severity].icon}<Typography variant="body2">{severity[row.severity].label}</Typography></Stack>
                      </TableCell>
                      <TableCell>
                        <Link component={RouterLink} to={`/workloads/${encodeURIComponent(row.namespace)}/${encodeURIComponent(row.kind)}/${encodeURIComponent(row.workload)}`}>{row.workload}</Link>
                        <Typography variant="caption" color="text.secondary" component="div">{row.namespace} · {row.container} · {row.pods} pod{row.pods === 1 ? '' : 's'}</Typography>
                      </TableCell>
                      <TableCell align="right">{percent(row.throttlingRatio)}</TableCell>
                      <TableCell align="right">{percent(row.throttlingPeakRatio)}</TableCell>
                      <TableCell align="right">{row.spikeRatio !== undefined ? `${row.spikeRatio.toFixed(1)}×` : '—'}</TableCell>
                      <TableCell align="right">{row.cpuPeakMilli !== undefined ? formatMillicores(row.cpuPeakMilli) : '—'}</TableCell>
                      <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
                        {row.cpuRequestMilli ? formatMillicores(row.cpuRequestMilli) : 'unset'} / {row.cpuLimitMilli ? formatMillicores(row.cpuLimitMilli) : 'none'}
                        {row.pinnedAtLimit && <Chip size="small" variant="outlined" color="warning" label="pinned" sx={{ ml: 0.75 }} />}
                      </TableCell>
                      <TableCell>
                        <Typography variant="body2">{row.issues.join('; ')}</Typography>
                        <Typography variant="caption" color="text.secondary">{row.advice}</Typography>
                      </TableCell>
                      <TableCell align="right">
                        <Button size="small" startIcon={<AutoFixHighOutlined />} onClick={() => setGenerateFor({ namespace: row.namespace, kind: row.kind, workload: row.workload, container: row.container })}>Generate</Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          </CardContent>
        </Card>
      )}
      {generateFor && (
        <GenerateDialog key={`${generateFor.workload}/${generateFor.container}`} open initialSubject={generateFor} onClose={() => setGenerateFor(undefined)} onCreated={() => setGenerateFor(undefined)} />
      )}
    </Box>
  )
}
