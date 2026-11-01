import { useQuery } from '@tanstack/react-query'
import { Box, Card, CardContent, Chip, Skeleton, Stack, Typography } from '@mui/material'
import ArrowForward from '@mui/icons-material/ArrowForward'
import { api, type CapacityResponse } from '../api'
import { formatBytes, formatMillicores } from '../format'

/** Node impact of applying the current recommendations. */
export function CapacityCard() {
  const query = useQuery({
    queryKey: ['resource', 'capacity'],
    queryFn: ({ signal }) => api.get<CapacityResponse>('/api/v1/capacity', { signal }),
  })
  const data = query.data
  if (query.isLoading) return <Skeleton variant="rounded" height={160} sx={{ mb: 2 }} />
  if (!data || query.error) return null
  if (!data.status.available) {
    return (
      <Card sx={{ mb: 2 }}>
        <CardContent>
          <Typography variant="h6">Node impact</Typography>
          <Typography color="text.secondary">{data.status.reason}</Typography>
        </CardContent>
      </Card>
    )
  }
  const autoscaling = data.autoscaling
  return (
    <Card sx={{ mb: 2 }}>
      <CardContent>
        <Stack direction={{ xs: 'column', md: 'row' }} justifyContent="space-between" gap={2}>
          <Box>
            <Typography variant="h6">Node impact</Typography>
            <Typography variant="body2" color="text.secondary">Nodes needed to schedule every pod by its requests, now and with the newest recommendations applied.</Typography>
            <Stack direction="row" alignItems="center" gap={2} sx={{ mt: 2 }}>
              <Box>
                <Typography variant="caption" color="text.secondary">Now</Typography>
                <Typography variant="h3" component="p">{data.current?.nodesNeeded}</Typography>
              </Box>
              <ArrowForward color="action" aria-hidden />
              <Box>
                <Typography variant="caption" color="text.secondary">After</Typography>
                <Typography variant="h3" component="p">{data.recommended?.nodesNeeded}</Typography>
              </Box>
              <Box sx={{ ml: 2 }}>
                <Typography variant="caption" color="text.secondary">Reclaimable</Typography>
                <Typography variant="h3" component="p" color={data.reclaimableNodes ? 'success.main' : 'text.primary'}>{data.reclaimableNodes} node{data.reclaimableNodes === 1 ? '' : 's'}</Typography>
              </Box>
            </Stack>
            <Typography variant="caption" color="text.secondary">
              {data.nodes?.schedulable} of {data.nodes?.total} nodes schedulable · requested CPU {formatMillicores(data.current?.cpuRequestedMilli ?? 0)} → {formatMillicores(data.recommended?.cpuRequestedMilli ?? 0)} · memory {formatBytes(data.current?.memoryRequestedBytes ?? 0)} → {formatBytes(data.recommended?.memoryRequestedBytes ?? 0)}
              {(data.current?.unplacedPods ?? 0) > 0 && ` · ${data.current?.unplacedPods} pods do not fit current nodes by requests`}
            </Typography>
          </Box>
          <Box sx={{ maxWidth: { md: 520 } }}>
            <Stack direction="row" gap={1} flexWrap="wrap" sx={{ mb: 1 }}>
              {autoscaling?.karpenter && <Chip size="small" variant="outlined" color="success" label="Karpenter detected" />}
              {autoscaling?.clusterAutoscaler && <Chip size="small" variant="outlined" color="success" label="Cluster Autoscaler detected" />}
              {autoscaling?.nodePools.map((pool) => (
                <Chip key={pool.name} size="small" variant="outlined" label={`NodePool ${pool.name}: ${pool.consolidationPolicy}${pool.disruptionBudgets.length ? ` · budgets ${pool.disruptionBudgets.join(', ')}` : ''}`} />
              ))}
            </Stack>
            <Typography variant="body2">{data.realization}</Typography>
            <Typography variant="caption" color="text.secondary" component="p" sx={{ mt: 1 }}>{data.assumptions?.join(' ')}</Typography>
          </Box>
        </Stack>
      </CardContent>
    </Card>
  )
}
