import { useQuery } from '@tanstack/react-query'
import { Box, Card, CardContent, Chip, Skeleton, Stack, Table, TableBody, TableCell, TableContainer, TableHead, TableRow, Tooltip, Typography } from '@mui/material'
import ArrowForward from '@mui/icons-material/ArrowForward'
import { api, type CapacityResponse, type NodeGroupImpact } from '../api'
import { formatBytes, formatMillicores } from '../format'

const providerName: Record<NodeGroupImpact['provider'], string> = {
  karpenter: 'Karpenter NodePool',
  eks: 'EKS node group',
  gke: 'GKE node pool',
  aks: 'AKS agent pool',
  openshift: 'OpenShift MachineSet',
  'cluster-api': 'Cluster API MachineSet',
  none: 'No node group',
}

/** Per-group share of the node-impact estimate, with membership evidence. */
export function NodeGroupTable({ groups }: { groups: NodeGroupImpact[] }) {
  return (
    <TableContainer sx={{ mt: 2 }} tabIndex={0} role="region" aria-label="Node groups">
      <Table size="small" aria-label="Node impact by node group">
        <TableHead>
          <TableRow>
            <TableCell>Node group</TableCell>
            <TableCell>Membership</TableCell>
            <TableCell align="right">Nodes</TableCell>
            <TableCell align="right">Needed now → after</TableCell>
            <TableCell align="right">Reclaimable</TableCell>
            <TableCell>How it shrinks</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {groups.map((group) => (
            <TableRow key={`${group.provider}/${group.name}`}>
              <TableCell>
                <Typography variant="body2" sx={{ overflowWrap: 'anywhere' }}>{group.name}</Typography>
                <Typography variant="caption" color="text.secondary">
                  {providerName[group.provider]}{group.instanceTypes.length > 0 && ` · ${group.instanceTypes.join(', ')}`}{group.capacityTypes.length > 0 && ` · ${group.capacityTypes.join(', ')}`}
                </Typography>
              </TableCell>
              <TableCell>
                <Tooltip title={group.evidence}>
                  <Chip size="small" variant="outlined" color={group.membership === 'verified' ? 'success' : 'warning'} label={group.membership === 'verified' ? 'Verified' : 'Inferred'} />
                </Tooltip>
              </TableCell>
              <TableCell align="right">{group.schedulableNodes} of {group.nodes}</TableCell>
              <TableCell align="right">{group.currentNodesNeeded} → {group.recommendedNodesNeeded}</TableCell>
              <TableCell align="right">{group.reclaimableNodes}</TableCell>
              <TableCell><Typography variant="caption">{group.realization}</Typography></TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  )
}

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
              {(data.current?.noEligibleNodePods ?? 0) > 0 && `, ${data.current?.noEligibleNodePods} of them because no node matches their taints, nodeSelector, or required affinity`}
            </Typography>
            {(data.current?.unplacedWorkloads?.length ?? 0) > 0 && (
              <Typography variant="caption" color="text.secondary" component="p" sx={{ overflowWrap: 'anywhere' }}>
                Unplaced: {data.current?.unplacedWorkloads?.join(', ')}
              </Typography>
            )}
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
        {(data.nodeGroups?.length ?? 0) > 0 && <NodeGroupTable groups={data.nodeGroups ?? []} />}
      </CardContent>
    </Card>
  )
}
