import { useQuery } from '@tanstack/react-query'
import { Link as RouterLink } from 'react-router-dom'
import { Alert, Link, LinearProgress, Table, TableBody, TableCell, TableHead, TableRow, Typography } from '@mui/material'
import { api, ApiError, type WorkloadEfficiencyRow } from '../api'
import { formatBytes, formatCurrency, formatMillicores } from '../format'
import { UsageMeter } from './UsageMeter'

/** Per-workload right-sizing within one namespace, ranked by reclaimable CPU. */
export function WorkloadBreakdown({ namespace, usageWindow, currency }: { namespace: string; usageWindow: string; currency?: string }) {
  const query = useQuery({
    queryKey: ['resource', 'workload-efficiency', namespace, usageWindow],
    queryFn: ({ signal }) => api.get<{ items: WorkloadEfficiencyRow[]; status: { available: boolean; reason?: string } }>(`/api/v1/efficiency/workloads?namespace=${encodeURIComponent(namespace)}&window=${usageWindow}`, { signal }),
  })
  if (query.isLoading) return <LinearProgress aria-label={`Loading ${namespace} workloads`} />
  if (query.error) return <Alert severity="error">{query.error instanceof ApiError ? query.error.message : 'Workloads could not be loaded.'}</Alert>
  if (!query.data?.status.available) return <Alert severity="info">{query.data?.status.reason}</Alert>
  if (query.data.items.length === 0) return <Typography color="text.secondary">No workloads in this namespace.</Typography>
  return (
    <Table size="small" aria-label={`Workloads in ${namespace}`}>
      <TableHead>
        <TableRow>
          <TableCell>Workload</TableCell>
          <TableCell align="right">Replicas</TableCell>
          <TableCell sx={{ minWidth: 160 }}>CPU per pod</TableCell>
          <TableCell align="right">Reclaimable CPU</TableCell>
          <TableCell sx={{ minWidth: 160 }}>Memory per pod</TableCell>
          <TableCell align="right">Reclaimable memory</TableCell>
          {currency && <TableCell align="right">Per month</TableCell>}
        </TableRow>
      </TableHead>
      <TableBody>
        {query.data.items.map((row) => (
          <TableRow key={`${row.kind}/${row.name}`}>
            <TableCell>
              <Link component={RouterLink} to={`/workloads/${encodeURIComponent(namespace)}/${row.kind}/${encodeURIComponent(row.name)}`}>{row.name}</Link>
              <Typography variant="caption" color="text.secondary" component="div">{row.kind}{row.containersWithoutRequests ? ` · ${row.containersWithoutRequests} without requests` : ''}</Typography>
            </TableCell>
            <TableCell align="right">{row.replicas}</TableCell>
            <TableCell><UsageMeter label={`${row.name} CPU per pod`} requested={row.cpuRequestPerPodMilli} average={row.cpuAveragePerPodMilli} p95={row.cpuP95PerPodMilli} format={formatMillicores} /></TableCell>
            <TableCell align="right">{row.cpuReclaimableMilli !== undefined ? formatMillicores(row.cpuReclaimableMilli) : '—'}</TableCell>
            <TableCell><UsageMeter label={`${row.name} memory per pod`} requested={row.memoryRequestPerPodBytes} average={row.memoryAveragePerPodBytes} p95={row.memoryP95PerPodBytes} format={formatBytes} /></TableCell>
            <TableCell align="right">{row.memoryReclaimableBytes !== undefined ? formatBytes(row.memoryReclaimableBytes) : '—'}</TableCell>
            {currency && <TableCell align="right">{row.reclaimableMonthlyCost !== undefined ? formatCurrency(row.reclaimableMonthlyCost, currency) : '—'}</TableCell>}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
