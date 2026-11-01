import { useQuery } from '@tanstack/react-query'
import { Alert, Box, Card, CardContent, Chip, Skeleton, Stack, Typography } from '@mui/material'
import CheckCircleOutline from '@mui/icons-material/CheckCircleOutline'
import ErrorOutline from '@mui/icons-material/ErrorOutline'
import RemoveCircleOutline from '@mui/icons-material/RemoveCircleOutline'
import { api, ApiError, type DependencyStatus } from '../api'
import { PageHeader } from './PageHeader'

function state(dependency: DependencyStatus) {
  if (!dependency.configured) return { label: 'Not configured', color: 'default' as const, icon: <RemoveCircleOutline fontSize="small" /> }
  if (dependency.healthy) return { label: 'Healthy', color: 'success' as const, icon: <CheckCircleOutline fontSize="small" /> }
  return { label: 'Unavailable', color: dependency.critical ? 'error' as const : 'warning' as const, icon: <ErrorOutline fontSize="small" /> }
}

export function Integrations({ title, description }: { title: string; description: string }) {
  const query = useQuery({
    queryKey: ['resource', 'integrations'],
    queryFn: ({ signal }) => api.get<{ items: DependencyStatus[] }>('/api/v1/settings/integrations', { signal }),
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
                <Stack direction="row" justifyContent="space-between" alignItems="center" gap={1}>
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
    </Box>
  )
}
