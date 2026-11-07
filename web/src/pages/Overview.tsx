import { useQuery } from '@tanstack/react-query'
import { Link as RouterLink } from 'react-router-dom'
import { Alert, Box, Button, Card, CardActionArea, CardContent, Chip, LinearProgress, Skeleton, Stack, Typography } from '@mui/material'
import CheckCircleOutline from '@mui/icons-material/CheckCircleOutline'
import ShieldOutlined from '@mui/icons-material/ShieldOutlined'
import SavingsOutlined from '@mui/icons-material/SavingsOutlined'
import LanOutlined from '@mui/icons-material/LanOutlined'
import { api, type ApiRecord } from '../api'
import { RequestAlert } from '../components/RequestAlert'

type OverviewResponse = ApiRecord & {
  clusters?: number
  workloads?: number
  mode?: string
  inventory?: { available: boolean; reason?: string; source?: string }
  generatedAt?: string
  recommendations?: { total?: number; pending?: number; approved?: number; rejected?: number; source?: string }
  estimatedMonthlySavings?: number
  currency?: string
  dependencies?: Array<{ name: string; healthy: boolean; configured?: boolean; critical?: boolean; detail?: string }>
}

const metricCards = [
  { key: 'clusters', label: 'Connected clusters', icon: <LanOutlined />, path: '/clusters' },
  { key: 'workloads', label: 'Observed workloads', icon: <CheckCircleOutline />, path: '/workloads' },
  { key: 'pending', label: 'Awaiting review', icon: <ShieldOutlined />, path: '/approvals' },
] as const

export function Overview() {
  const query = useQuery({ queryKey: ['overview'], queryFn: ({ signal }) => api.get<OverviewResponse>('/api/v1/overview', { signal }), retry: 1, refetchInterval: 60_000 })
  const value = query.data
  const metricValue = (key: typeof metricCards[number]['key']): number | undefined => {
    const candidate = key === 'pending' ? value?.recommendations?.pending : value?.[key]
    return typeof candidate === 'number' ? candidate : undefined
  }
  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <Box sx={{ mb: 4 }}>
        <Typography variant="overline" color="primary.main">FLEET INTELLIGENCE</Typography>
        <Typography variant="h2" component="h1" sx={{ maxWidth: 820 }}>Rightsize with evidence,<br />not instinct.</Typography>
        <Typography color="text.secondary" sx={{ mt: 1.5, maxWidth: 700 }}>KubeRight separates observed signals, forecasts, and estimated savings so platform teams can make controlled changes with a clear audit trail.</Typography>
        <Stack direction={{ xs: 'column', sm: 'row' }} gap={1} sx={{ mt: 2.5 }} alignItems={{ sm: 'center' }}>
          <Button component={RouterLink} to="/workloads" variant="contained">Choose a workload</Button>
          <Button component={RouterLink} to="/approvals" variant="outlined">Review decision queue</Button>
          {value?.generatedAt && <Typography variant="caption" color="text.secondary" sx={{ ml: { sm: 1 } }}>Control-plane snapshot {new Date(value.generatedAt).toLocaleString()}</Typography>}
        </Stack>
      </Box>
      {query.error && <Box sx={{ mb: 2 }}><RequestAlert error={query.error} resource="fleet overview" onRetry={() => void query.refetch()} /></Box>}
      {value?.inventory?.available === false && <Alert severity="info" sx={{ mb: 2 }}>{value.inventory.reason ?? 'No provider has confirmed fleet data.'}</Alert>}
      <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', sm: 'repeat(2, 1fr)', xl: 'repeat(4, 1fr)' }, gap: 2 }}>
        {metricCards.map((card) => (
          <Card key={card.key}>
            <CardActionArea component={RouterLink} to={card.path} aria-label={`Open ${card.label.toLowerCase()}`} sx={{ height: '100%' }}>
              <CardContent>
                <Stack direction="row" justifyContent="space-between" alignItems="center"><Typography color="text.secondary">{card.label}</Typography><Box color="primary.main">{card.icon}</Box></Stack>
                {query.isLoading ? <Skeleton width="45%" height={60} /> : <Typography variant="h3" sx={{ mt: 2 }}>{typeof metricValue(card.key) === 'number' ? metricValue(card.key) : '—'}</Typography>}
                <Typography variant="caption" color="text.secondary">Confirmed by the control plane · open</Typography>
              </CardContent>
            </CardActionArea>
          </Card>
        ))}
        <Card>
          <CardContent>
            <Stack direction="row" justifyContent="space-between"><Typography color="text.secondary">Estimated monthly savings</Typography><SavingsOutlined color="secondary" /></Stack>
            {query.isLoading ? <Skeleton width="60%" height={60} /> : <Typography variant="h3" sx={{ mt: 2 }}>{typeof value?.estimatedMonthlySavings === 'number' ? new Intl.NumberFormat(undefined, { style: 'currency', currency: value.currency ?? 'USD', maximumFractionDigits: 0 }).format(value.estimatedMonthlySavings) : '—'}</Typography>}
            <Typography variant="caption" color="text.secondary">Estimate only; not realized billing savings</Typography>
          </CardContent>
        </Card>
      </Box>
      <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', lg: '1.35fr .65fr' }, gap: 2, mt: 2 }}>
        <Card>
          <CardContent>
            <Stack direction="row" justifyContent="space-between" alignItems="center" sx={{ mb: 2 }}><Typography variant="h6">Dependency posture</Typography>{value?.generatedAt && <Typography variant="caption" color="text.secondary">As of {new Date(value.generatedAt).toLocaleString()}</Typography>}</Stack>
            {query.isLoading ? <LinearProgress /> : !value?.dependencies?.length ? <Typography color="text.secondary">No dependency status has been reported.</Typography> : value.dependencies.map((dependency) => (
              <Stack key={dependency.name} direction="row" justifyContent="space-between" alignItems="center" gap={1.5} sx={{ py: 1.25, borderBottom: '1px solid', borderColor: 'divider' }}>
                <Box sx={{ minWidth: 0 }}><Typography sx={{ fontWeight: 700 }}>{dependency.name}</Typography><Typography variant="caption" color="text.secondary" sx={{ overflowWrap: 'anywhere' }}>{dependency.detail ?? 'No detail supplied'}</Typography></Box>
                <Chip size="small" label={dependency.healthy ? 'Healthy' : dependency.configured === false ? 'Not configured' : 'Unavailable'} color={dependency.healthy ? 'success' : dependency.configured === false ? 'default' : 'warning'} variant="outlined" sx={{ flexShrink: 0 }} />
              </Stack>
            ))}
          </CardContent>
        </Card>
        <Card sx={{ background: 'linear-gradient(150deg, rgba(41,124,112,.3), rgba(10,25,38,.98))' }}>
          <CardContent>
            <Typography variant="overline">SAFETY ENVELOPE</Typography>
            <Typography variant="h5" sx={{ mt: 1 }}>Observe → Recommend → Approve</Typography>
            <Typography color="text.secondary" sx={{ my: 2 }}>Recommendations are explainable proposals. Reviewers approve or reject them; approved changes are applied through your own change process. KubeRight never mutates workloads.</Typography>
            <Chip label={value?.mode ? `${value.mode} mode` : 'Mode unconfirmed'} icon={<ShieldOutlined />} color="warning" variant="outlined" />
          </CardContent>
        </Card>
      </Box>
    </Box>
  )
}
