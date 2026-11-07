import { Fragment, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Alert, Box, Button, Card, CardContent, IconButton, LinearProgress, MenuItem, Skeleton, Stack, Table, TableBody, TableCell,
  TableContainer, TableHead, TableRow, TextField, Typography,
} from '@mui/material'
import WarningAmberOutlined from '@mui/icons-material/WarningAmberOutlined'
import KeyboardArrowDown from '@mui/icons-material/KeyboardArrowDown'
import KeyboardArrowUp from '@mui/icons-material/KeyboardArrowUp'
import { api, ApiError, type EfficiencyResponse } from '../api'
import { AnalyzeDialog } from '../components/AnalyzeDialog'
import { CapacityCard } from '../components/CapacityCard'
import { UsageMeter } from '../components/UsageMeter'
import { WorkloadBreakdown } from '../components/WorkloadBreakdown'
import { formatBytes, formatCurrency, formatMillicores, formatRatio } from '../format'
import { PageHeader } from './PageHeader'

const windows = [
  { value: '24h', label: '24 hours' },
  { value: '72h', label: '3 days' },
  { value: '168h', label: '7 days' },
  { value: '720h', label: '30 days' },
]

function StatTile({ label, value, note, warning }: { label: string; value: string; note: string; warning?: boolean }) {
  return (
    <Card>
      <CardContent>
        <Typography color="text.secondary" variant="body2">{label}</Typography>
        <Stack direction="row" alignItems="center" gap={1} sx={{ mt: 1 }}>
          {warning && <WarningAmberOutlined color="warning" aria-hidden />}
          <Typography variant="h4" component="p" sx={{ fontWeight: 750 }}>{value}</Typography>
        </Stack>
        <Typography variant="caption" color="text.secondary">{note}</Typography>
      </CardContent>
    </Card>
  )
}

export function Efficiency({ title, description }: { title: string; description: string }) {
  const [usageWindow, setUsageWindow] = useState('24h')
  const [analyze, setAnalyze] = useState<string>()
  const [expanded, setExpanded] = useState<string>()
  const query = useQuery({
    queryKey: ['resource', 'efficiency', usageWindow],
    queryFn: ({ signal }) => api.get<EfficiencyResponse>(`/api/v1/efficiency?window=${usageWindow}`, { signal }),
  })
  const data = query.data
  const summary = data?.summary

  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <PageHeader eyebrow="FLEET EFFICIENCY" title={title} description={description}>
        <Stack direction="row" gap={1.5} alignItems="center" sx={{ mt: 2 }}>
          <TextField select size="small" label="Usage window" value={usageWindow} onChange={(event) => setUsageWindow(event.target.value)} sx={{ minWidth: 160 }}>
            {windows.map((option) => <MenuItem key={option.value} value={option.value}>{option.label}</MenuItem>)}
          </TextField>
        </Stack>
      </PageHeader>
      {query.isFetching && <LinearProgress sx={{ mb: 2 }} />}
      {query.error && <Alert severity="error" sx={{ mb: 2 }}>{query.error instanceof ApiError ? query.error.message : 'Efficiency could not be calculated.'}</Alert>}
      {data?.status.available === false && <Alert severity="info" sx={{ mb: 2 }}>{data.status.reason}</Alert>}

      <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', sm: 'repeat(2, 1fr)', lg: 'repeat(3, 1fr)', xl: 'repeat(6, 1fr)' }, gap: 2, mb: 2 }}>
        {query.isLoading && Array.from({ length: 6 }, (_, index) => <Skeleton key={index} variant="rounded" height={118} />)}
        {summary && (
          <>
            <StatTile label="CPU efficiency" value={formatRatio(summary.cpuEfficiency)} note="Average usage ÷ requested" />
            <StatTile label="Memory efficiency" value={formatRatio(summary.memoryEfficiency)} note="Average usage ÷ requested" />
            <StatTile label="Reclaimable CPU" value={formatMillicores(summary.cpuReclaimableMilli)} note="Requested minus P95 usage" />
            <StatTile label="Reclaimable memory" value={formatBytes(summary.memoryReclaimableBytes)} note="Requested minus P95 usage" />
            <StatTile
              label="Reclaimable per month"
              value={summary.reclaimableMonthlyCost !== undefined ? formatCurrency(summary.reclaimableMonthlyCost, summary.currency) : '—'}
              note={summary.reclaimableMonthlyCost !== undefined ? 'At configured unit prices' : 'Unit prices are not configured'}
            />
            <StatTile
              label="Containers without requests"
              value={summary.containersWithoutRequests.toLocaleString()}
              note={summary.containersWithoutRequests ? 'Unbounded; schedule unpredictably' : 'Every container sets requests'}
              warning={summary.containersWithoutRequests > 0}
            />
          </>
        )}
      </Box>

      {data?.status.available && <CapacityCard />}

      {data && data.items.length > 0 && (
        <Card>
          <CardContent>
            <Typography variant="h6" sx={{ mb: 1 }}>Namespaces by reclaimable CPU</Typography>
            <Typography variant="caption" color="text.secondary" component="p" sx={{ mb: 2 }}>
              Track: requested. Fill: average usage. Tick: P95 usage. {summary?.basis}.
            </Typography>
            <TableContainer>
              <Table size="small" aria-label="Namespace efficiency">
                <TableHead>
                  <TableRow>
                    <TableCell sx={{ fontWeight: 800 }}>Namespace</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">Pods</TableCell>
                    <TableCell sx={{ fontWeight: 800, minWidth: 180 }}>CPU</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">Reclaimable CPU</TableCell>
                    <TableCell sx={{ fontWeight: 800, minWidth: 180 }}>Memory</TableCell>
                    <TableCell sx={{ fontWeight: 800 }} align="right">Reclaimable memory</TableCell>
                    {summary?.currency && <TableCell sx={{ fontWeight: 800 }} align="right">Per month</TableCell>}
                    <TableCell sx={{ fontWeight: 800 }} align="right">No requests</TableCell>
                    <TableCell />
                  </TableRow>
                </TableHead>
                <TableBody>
                  {data.items.map((row) => (
                    <Fragment key={row.namespace}>
                    <TableRow hover>
                      <TableCell sx={{ fontWeight: 700, whiteSpace: 'nowrap' }}>
                        <IconButton size="small" aria-label={`${expanded === row.namespace ? 'Hide' : 'Show'} workloads in ${row.namespace}`} aria-expanded={expanded === row.namespace} onClick={() => setExpanded(expanded === row.namespace ? undefined : row.namespace)}>
                          {expanded === row.namespace ? <KeyboardArrowUp fontSize="small" /> : <KeyboardArrowDown fontSize="small" />}
                        </IconButton>
                        {row.namespace}
                      </TableCell>
                      <TableCell align="right">{row.pods}</TableCell>
                      <TableCell><UsageMeter label={`${row.namespace} CPU`} requested={row.cpuRequestedMilli} average={row.cpuAverageMilli} p95={row.cpuP95Milli} format={formatMillicores} /></TableCell>
                      <TableCell align="right">{row.cpuReclaimableMilli !== undefined ? formatMillicores(row.cpuReclaimableMilli) : '—'}</TableCell>
                      <TableCell><UsageMeter label={`${row.namespace} memory`} requested={row.memoryRequestedBytes} average={row.memoryAverageBytes} p95={row.memoryP95Bytes} format={formatBytes} /></TableCell>
                      <TableCell align="right">{row.memoryReclaimableBytes !== undefined ? formatBytes(row.memoryReclaimableBytes) : '—'}</TableCell>
                      {summary?.currency && <TableCell align="right">{row.reclaimableMonthlyCost !== undefined ? formatCurrency(row.reclaimableMonthlyCost, summary.currency) : '—'}</TableCell>}
                      <TableCell align="right">{row.containersWithoutRequests || '—'}</TableCell>
                      <TableCell align="right"><Button size="small" onClick={() => setAnalyze(row.namespace)}>Analyze</Button></TableCell>
                    </TableRow>
                    {expanded === row.namespace && (
                      <TableRow>
                        <TableCell colSpan={summary?.currency ? 9 : 8} sx={{ bgcolor: 'rgba(255,255,255,.02)', py: 2 }}>
                          <WorkloadBreakdown namespace={row.namespace} usageWindow={usageWindow} currency={summary?.currency} />
                        </TableCell>
                      </TableRow>
                    )}
                    </Fragment>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          </CardContent>
        </Card>
      )}
      <AnalyzeDialog key={analyze} open={Boolean(analyze)} namespace={analyze} onClose={() => setAnalyze(undefined)} />
    </Box>
  )
}
