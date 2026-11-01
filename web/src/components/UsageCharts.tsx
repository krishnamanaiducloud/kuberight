import { useQuery } from '@tanstack/react-query'
import { Alert, Card, CardContent, Chip, LinearProgress, Stack, Typography } from '@mui/material'
import { api, ApiError, type HistoryResponse, type Subject, type UsageSeries } from '../api'
import { seriesColors } from '../chartColors'
import { formatBytes, formatMillicores } from '../format'
import { LineChart, type ChartReference } from './LineChart'

function QualityChips({ series }: { series: UsageSeries }) {
  const issues = series.quality.issues ?? []
  return (
    <Stack direction="row" gap={1} flexWrap="wrap">
      <Chip size="small" variant="outlined" label={`Coverage ${Math.round(series.quality.coverage * 100)}%`} color={series.quality.coverage >= 0.8 ? 'success' : 'warning'} />
      {issues.map((issue) => <Chip key={issue} size="small" variant="outlined" color="warning" label={issue} />)}
    </Stack>
  )
}

function UsageCard({ title, series, references, format }: { title: string; series?: UsageSeries; references: ChartReference[]; format: (value: number) => string }) {
  return (
    <Card>
      <CardContent>
        <Stack direction={{ xs: 'column', sm: 'row' }} justifyContent="space-between" alignItems={{ sm: 'center' }} gap={1} sx={{ mb: 2 }}>
          <Typography variant="h6">{title}</Typography>
          {series && <QualityChips series={series} />}
        </Stack>
        <LineChart
          title={title}
          series={series ? [{ name: 'Busiest replica', color: seriesColors[0], points: series.points.map((point) => ({ time: Date.parse(point.timestamp), value: point.value })) }] : []}
          references={references}
          format={format}
        />
      </CardContent>
    </Card>
  )
}

/** CPU and memory history for one container against its requests and limits. */
export function UsageCharts({ subject, usageWindow }: { subject: Subject; usageWindow: string }) {
  const query = useQuery({
    queryKey: ['history', subject, usageWindow],
    queryFn: ({ signal }) => {
      const parameters = new URLSearchParams({ ...subject, window: usageWindow })
      return api.get<HistoryResponse>(`/api/v1/metrics/history?${parameters}`, { signal })
    },
  })
  const data = query.data
  const cpu = data?.items.find((item) => item.resource === 'cpu')
  const memory = data?.items.find((item) => item.resource === 'memory')
  const current = data?.current
  const cpuReferences = [
    ...(current?.cpuRequestMilli ? [{ label: 'Request', value: current.cpuRequestMilli }] : []),
    ...(current?.cpuLimitMilli ? [{ label: 'Limit', value: current.cpuLimitMilli }] : []),
  ]
  const memoryReferences = [
    ...(current?.memoryRequestBytes ? [{ label: 'Request', value: current.memoryRequestBytes }] : []),
    ...(current?.memoryLimitBytes ? [{ label: 'Limit', value: current.memoryLimitBytes }] : []),
  ]
  return (
    <Stack gap={2}>
      {query.isFetching && <LinearProgress />}
      {query.error && <Alert severity="error">{query.error instanceof ApiError ? query.error.message : 'Usage history could not be loaded.'}</Alert>}
      {data?.status && (data.status.available === false || data.status.reason) && <Alert severity="info">{data.status.reason}</Alert>}
      {data && data.status.available && (
        <>
          <UsageCard title="CPU" series={cpu} references={cpuReferences} format={formatMillicores} />
          <UsageCard title="Memory working set" series={memory} references={memoryReferences} format={formatBytes} />
          <Typography variant="caption" color="text.secondary">
            Each point is the busiest replica at that step{data.step ? ` (${data.step} resolution)` : ''}. Missing samples are gaps, never zeros.
            {!current && ' Current requests and limits are unavailable because inventory did not return this container.'}
          </Typography>
        </>
      )}
    </Stack>
  )
}
