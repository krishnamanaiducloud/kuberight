import { Table, TableBody, TableCell, TableHead, TableRow } from '@mui/material'
import type { RecommendationRecord } from '../api'
import { formatBytes, formatMillicores, formatPercent } from '../format'

export type ResourceFields = Pick<RecommendationRecord,
  'cpuRequestMilli' | 'cpuLimitMilli' | 'memoryRequestBytes' | 'memoryLimitBytes' |
  'currentCpuRequestMilli' | 'currentCpuLimitMilli' | 'currentMemoryRequestBytes' | 'currentMemoryLimitBytes'>

function change(current: number, recommended: number) {
  if (current <= 0) return recommended > 0 ? 'new value' : '—'
  return formatPercent(((recommended - current) / current) * 100)
}

/** Current versus recommended requests and limits, with the change. */
export function ResourceTable({ record }: { record: ResourceFields }) {
  const rows = [
    { label: 'CPU request', current: record.currentCpuRequestMilli, next: record.cpuRequestMilli, format: formatMillicores },
    { label: 'CPU limit', current: record.currentCpuLimitMilli, next: record.cpuLimitMilli, format: formatMillicores },
    { label: 'Memory request', current: record.currentMemoryRequestBytes, next: record.memoryRequestBytes, format: formatBytes },
    { label: 'Memory limit', current: record.currentMemoryLimitBytes, next: record.memoryLimitBytes, format: formatBytes },
  ]
  return (
    <Table size="small" aria-label="Current and recommended resources">
      <TableHead><TableRow><TableCell>Resource</TableCell><TableCell align="right">Current</TableCell><TableCell align="right">Recommended</TableCell><TableCell align="right">Change</TableCell></TableRow></TableHead>
      <TableBody>
        {rows.map((row) => (
          <TableRow key={row.label}>
            <TableCell>{row.label}</TableCell>
            <TableCell align="right">{row.current > 0 ? row.format(row.current) : 'unset'}</TableCell>
            <TableCell align="right" sx={{ fontWeight: 750 }}>{row.next > 0 ? row.format(row.next) : 'unset'}</TableCell>
            <TableCell align="right">{change(row.current, row.next)}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
