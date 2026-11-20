import { useQuery } from '@tanstack/react-query'
import { Alert, Box, Card, CardContent, Chip, Divider, Skeleton, Stack, Typography } from '@mui/material'
import CheckCircleOutline from '@mui/icons-material/CheckCircleOutlineOutlined'
import ErrorOutline from '@mui/icons-material/ErrorOutlineOutlined'
import RemoveCircleOutline from '@mui/icons-material/RemoveCircleOutlineOutlined'
import HelpOutline from '@mui/icons-material/HelpOutlineOutlined'
import { api, ApiError, type DependencyStatus, type MetricCoverage } from '../api'
import { PageHeader } from './PageHeader'

const stateLabels: Record<string, string> = {
  connected_metrics_missing: 'Metrics missing',
  authentication_failed: 'Authentication failed',
  authorization_denied: 'Access denied',
  query_invalid: 'Query rejected',
  timeout: 'Timed out',
  insufficient_data: 'Insufficient data',
}

function state(dependency: DependencyStatus) {
  if (!dependency.configured || dependency.state === 'disabled') return { label: 'Not configured', color: 'default' as const, icon: <RemoveCircleOutline fontSize="small" /> }
  if (dependency.healthy) return { label: 'Healthy', color: 'success' as const, icon: <CheckCircleOutline fontSize="small" /> }
  const label = (dependency.state && stateLabels[dependency.state]) || 'Unavailable'
  return { label, color: dependency.critical ? 'error' as const : 'warning' as const, icon: <ErrorOutline fontSize="small" /> }
}

function coverageState(item: MetricCoverage) {
  if (item.state === 'available') return { label: 'Available', color: 'success' as const, icon: <CheckCircleOutline fontSize="small" /> }
  if (item.state === 'unavailable') return { label: 'Missing', color: 'warning' as const, icon: <ErrorOutline fontSize="small" /> }
  return { label: 'Unknown', color: 'default' as const, icon: <HelpOutline fontSize="small" /> }
}

function MetricCoverageCard({ coverage }: { coverage: MetricCoverage[] }) {
  return (
    <Card sx={{ mt: 3 }}>
      <CardContent>
        <Typography variant="h6" component="h2">Prometheus metric coverage</Typography>
        <Typography color="text.secondary" variant="body2" sx={{ mb: 1 }}>
          Series scraped in the last 10 minutes. Missing metrics are shown as missing; KubeRight never treats them as zero usage, and recommendations that need them report insufficient data.
        </Typography>
        <Stack divider={<Divider flexItem />}>
          {coverage.map((item) => {
            const current = coverageState(item)
            return (
              <Stack key={item.metric} direction={{ xs: 'column', sm: 'row' }} sx={{ gap: { xs: 0.5, sm: 2 }, alignItems: { sm: 'center' }, py: 1.25 }}>
                <Box sx={{ minWidth: { sm: 200 } }}>
                  <Typography sx={{ fontWeight: 600 }}>{item.signal}</Typography>
                  <Typography variant="caption" color="text.secondary">{item.source}</Typography>
                </Box>
                <Box sx={{ flex: 1, minWidth: 0 }}>
                  <Typography variant="body2" sx={{ fontFamily: 'monospace', overflowWrap: 'anywhere' }}>{item.metric}</Typography>
                  <Typography variant="caption" color="text.secondary">Used for {item.usedFor}{item.note ? `. ${item.note}` : ''}</Typography>
                </Box>
                <Stack direction="row" sx={{ gap: 1, alignItems: 'center', minWidth: { sm: 210 }, justifyContent: { sm: 'flex-end' } }}>
                  {item.state === 'available' && <Typography variant="body2" color="text.secondary">{item.series.toLocaleString()} series</Typography>}
                  <Chip size="small" variant="outlined" icon={current.icon} label={current.label} color={current.color} />
                </Stack>
              </Stack>
            )
          })}
        </Stack>
      </CardContent>
    </Card>
  )
}

export function Integrations({ title, description }: { title: string; description: string }) {
  const query = useQuery({
    queryKey: ['resource', 'integrations'],
    queryFn: ({ signal }) => api.get<{ items: DependencyStatus[]; metricCoverage?: MetricCoverage[] }>('/api/v1/settings/integrations', { signal }),
    refetchInterval: 30_000,
  })
  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <PageHeader eyebrow="RUNTIME CONFIGURATION" title={title} description={description} />
      {query.error && <Alert severity="error" sx={{ mb: 2 }}>{query.error instanceof ApiError ? query.error.message : 'Integration status could not be loaded.'}</Alert>}
      <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', md: 'repeat(2, 1fr)', xl: 'repeat(3, 1fr)' }, gap: 2 }}>
        {query.isLoading && [0, 1, 2].map((index) => <Skeleton key={index} variant="rounded" height={120} />)}
        {query.data?.items.map((dependency) => {
          const current = state(dependency)
          return (
            <Card key={dependency.name}>
              <CardContent>
                <Stack direction="row" sx={{ justifyContent: 'space-between', alignItems: 'center', gap: 1 }}>
                  <Typography variant="h6">{dependency.name}</Typography>
                  <Chip size="small" variant="outlined" icon={current.icon} label={current.label} color={current.color} />
                </Stack>
                <Typography color="text.secondary" sx={{ mt: 1 }}>{dependency.detail}</Typography>
                <Typography variant="caption" color="text.secondary">{dependency.critical ? 'Required for core API operation' : 'Optional enrichment'}</Typography>
              </CardContent>
            </Card>
          )
        })}
      </Box>
      {query.data?.metricCoverage && <MetricCoverageCard coverage={query.data.metricCoverage} />}
    </Box>
  )
}
