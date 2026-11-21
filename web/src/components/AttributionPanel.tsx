import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Accordion, AccordionDetails, AccordionSummary, Alert, AlertTitle, Box, Chip, Skeleton, Stack, Table, TableBody, TableCell,
  TableContainer, TableHead, TableRow, Typography,
} from '@mui/material'
import ExpandMoreIcon from '@mui/icons-material/ExpandMore'
import AccountTreeOutlined from '@mui/icons-material/AccountTreeOutlined'
import { api, type AttributionDiagnostics, type AttributionOutcome, type Subject } from '../api'
import { formatDate } from '../format'
import { RequestAlert } from './RequestAlert'

const outcomes: Record<AttributionOutcome, { label: string; severity: 'success' | 'info' | 'warning' | 'error' }> = {
  attributed: { label: 'Usage attributed to this workload', severity: 'success' },
  insufficient_history: { label: 'Insufficient usage history', severity: 'info' },
  missing_metrics: { label: 'Required metrics are missing', severity: 'warning' },
  incomplete_attribution: { label: 'Workload attribution is incomplete', severity: 'warning' },
  provider_unavailable: { label: 'Prometheus is unavailable', severity: 'error' },
}

const shortUID = (uid?: string) => (uid ? uid.slice(0, 8) : '—')

function Fact({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <Box sx={{ minWidth: 0 }}>
      <Typography variant="caption" color="text.secondary" component="div">{label}</Typography>
      <Typography variant="body2" sx={{ fontWeight: 700, overflowWrap: 'anywhere', fontFamily: mono ? 'monospace' : undefined }}>{value}</Typography>
    </Box>
  )
}

/**
 * Read-only evidence for how Prometheus usage is tied to this workload: the
 * UID chain from the Kubernetes API, every pod identity with the reason it
 * was or was not attributed, metric family availability, and coverage. It is
 * fetched only when opened because it runs about twenty Prometheus queries.
 */
export function AttributionPanel({ subject, usageWindow }: { subject: Subject; usageWindow: string }) {
  const [open, setOpen] = useState(false)
  // Attribution windows are bounded like recommendation windows (1h-720h);
  // callers pass analysisWindow().
  const window = usageWindow
  const query = useQuery({
    queryKey: ['attribution', subject, window],
    enabled: open,
    staleTime: 60_000,
    queryFn: ({ signal }) => api.get<AttributionDiagnostics>(`/api/v1/attribution?${new URLSearchParams({ ...subject, window })}`, { signal }),
  })
  const data = query.data
  const report = data?.report
  const outcome = report ? outcomes[report.outcome] : undefined

  return (
    <Accordion expanded={open} onChange={(_, next) => setOpen(next)} disableGutters variant="outlined" sx={{ mb: 2, borderRadius: 1, '&::before': { display: 'none' } }}>
      <AccordionSummary expandIcon={<ExpandMoreIcon />} aria-controls="attribution-details" id="attribution-summary">
        <Stack direction="row" sx={{ gap: 1, alignItems: 'center', flexWrap: 'wrap' }}>
          <AccountTreeOutlined color="primary" aria-hidden />
          <Typography variant="h6" component="h2">Metric attribution</Typography>
          {outcome && <Chip size="small" variant="outlined" color={outcome.severity} label={outcome.label} />}
        </Stack>
      </AccordionSummary>
      <AccordionDetails id="attribution-details">
        {query.isLoading && <Skeleton variant="rounded" height={160} />}
        {query.error && <RequestAlert error={query.error} resource="attribution diagnostics" namespaceScoped onRetry={() => void query.refetch()} />}
        {data && report && outcome && (
          <Stack sx={{ gap: 2 }}>
            <Alert severity={outcome.severity} variant="outlined">
              <AlertTitle>{outcome.label}</AlertTitle>
              {report.reason}
              {report.eligible && report.outcome !== 'attributed' && ' A recommendation can still be calculated, but reductions stay blocked until coverage is complete.'}
            </Alert>

            <Box sx={{ display: 'grid', gap: 2, gridTemplateColumns: { xs: 'repeat(2, minmax(0, 1fr))', md: 'repeat(4, minmax(0, 1fr))' } }}>
              <Fact label={`${data.identity.kind} UID`} value={data.identity.uid} mono />
              <Fact label="Created" value={formatDate(data.identity.createdAt)} />
              <Fact label="Coverage (replica-time)" value={`${report.coveragePercent.toFixed(1)}%`} />
              <Fact label="Pods with usage / attributed" value={`${report.usagePods} / ${report.attributedPods}`} />
              <Fact label="Unattributed / excluded pods" value={`${report.unattributedPods} / ${report.excludedPods ?? 0}`} />
              <Fact label="Window" value={`${formatDate(report.start)} – ${formatDate(report.end)}`} />
              <Fact label="Step" value={report.step} />
              <Fact label="Recommendation" value={report.eligible ? 'Can be calculated' : 'Not possible yet'} />
            </Box>

            {data.identity.resolutionIssues.length > 0 && (
              <Alert severity="warning">{data.identity.resolutionIssues.join('; ')}</Alert>
            )}
            {(report.coverage?.issues.length ?? 0) > 0 && (
              <Box>
                <Typography variant="subtitle2" sx={{ mb: 0.5 }}>Coverage issues</Typography>
                <Box component="ul" sx={{ m: 0, pl: 2.5 }}>
                  {report.coverage?.issues.map((issue) => <Typography component="li" variant="body2" key={issue}>{issue}</Typography>)}
                </Box>
              </Box>
            )}

            {report.owners.length > 0 && (
              <Box>
                <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
                  {report.owners[0].kind === 'Job' ? 'Jobs' : 'ReplicaSets'} linked to {data.identity.kind} {data.identity.name}
                </Typography>
                <TableContainer>
                  <Table size="small" aria-label="Owners linked to the workload">
                    <TableHead><TableRow><TableCell>Name</TableCell><TableCell>UID</TableCell><TableCell>Evidence</TableCell><TableCell>Created</TableCell><TableCell align="right">Pods</TableCell></TableRow></TableHead>
                    <TableBody>
                      {report.owners.map((owner) => (
                        <TableRow key={owner.name}>
                          <TableCell sx={{ overflowWrap: 'anywhere' }}>{owner.name}</TableCell>
                          <TableCell sx={{ fontFamily: 'monospace' }} title={owner.uid}>{shortUID(owner.uid)}</TableCell>
                          <TableCell>
                            <Stack direction="row" sx={{ gap: 0.5, flexWrap: 'wrap' }}>
                              {owner.verified && <Chip size="small" variant="outlined" color="success" label="API owner UID" />}
                              {owner.recorded && <Chip size="small" variant="outlined" label="kube-state-metrics" />}
                            </Stack>
                          </TableCell>
                          <TableCell>{owner.createdAt ? formatDate(owner.createdAt) : '—'}</TableCell>
                          <TableCell align="right">{owner.replicas ?? '—'}</TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </TableContainer>
              </Box>
            )}

            <Box>
              <Typography variant="subtitle2" sx={{ mb: 0.5 }}>Pod identities in the window{report.podsTruncated ? ' (first 200)' : ''}</Typography>
              {report.pods.length === 0 ? <Typography variant="body2" color="text.secondary">No pod matching this workload exported usage or ownership series in the window.</Typography> : (
                <TableContainer sx={{ maxHeight: 360 }}>
                  <Table size="small" stickyHeader aria-label="Pod attribution">
                    <TableHead><TableRow><TableCell>Pod</TableCell><TableCell>UID</TableCell><TableCell>Owner</TableCell><TableCell>Created</TableCell><TableCell>Result</TableCell></TableRow></TableHead>
                    <TableBody>
                      {report.pods.map((pod) => (
                        <TableRow key={`${pod.pod}/${pod.uid ?? ''}`}>
                          <TableCell sx={{ overflowWrap: 'anywhere', minWidth: 160 }}>
                            {pod.pod}
                            <Stack direction="row" sx={{ gap: 0.5, mt: 0.5, flexWrap: 'wrap' }}>
                              {pod.live && <Chip size="small" variant="outlined" label="live" />}
                              {pod.hasUsage && <Chip size="small" variant="outlined" label="usage" />}
                            </Stack>
                          </TableCell>
                          <TableCell sx={{ fontFamily: 'monospace' }} title={pod.uid}>{shortUID(pod.uid)}</TableCell>
                          <TableCell sx={{ overflowWrap: 'anywhere' }}>{pod.owner ? `${pod.ownerKind} ${pod.owner}` : '—'}</TableCell>
                          <TableCell>{pod.createdAt ? <>{formatDate(pod.createdAt)}<Typography variant="caption" color="text.secondary" component="div">{pod.createdFrom}</Typography></> : '—'}</TableCell>
                          <TableCell sx={{ minWidth: 200 }}>
                            {pod.attributed && <Chip size="small" color="success" variant="outlined" label="Attributed" />}
                            {!pod.attributed && pod.excluded && (
                              <Stack sx={{ gap: 0.5, alignItems: 'flex-start' }}>
                                <Chip size="small" variant="outlined" label="Excluded: predecessor" />
                                <Typography variant="body2" color="text.secondary">{pod.reason}</Typography>
                              </Stack>
                            )}
                            {!pod.attributed && !pod.excluded && <Typography variant="body2" color="warning.main">{pod.reason}</Typography>}
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </TableContainer>
              )}
            </Box>

            <Box>
              <Typography variant="subtitle2" sx={{ mb: 0.5 }}>Metric families</Typography>
              <TableContainer>
                <Table size="small" aria-label="Metric families">
                  <TableHead><TableRow><TableCell>Series</TableCell><TableCell>Used for</TableCell><TableCell align="right">Found</TableCell><TableCell>Status</TableCell></TableRow></TableHead>
                  <TableBody>
                    {report.families.map((family) => {
                      const missing = report.missingFamilies.some((name) => name.split(' or ').includes(family.name))
                      const status = family.series > 0 ? 'Present' : missing ? 'Missing' : family.required && family.alternative ? `Not exported; ${family.alternative} used` : 'Not exported (optional)'
                      return (
                        <TableRow key={family.name}>
                          <TableCell sx={{ fontFamily: 'monospace', overflowWrap: 'anywhere' }}>{family.name}</TableCell>
                          <TableCell>{family.purpose}</TableCell>
                          <TableCell align="right">{family.series}</TableCell>
                          <TableCell><Chip size="small" variant="outlined" color={family.series > 0 ? 'success' : missing ? 'warning' : 'default'} label={status} /></TableCell>
                        </TableRow>
                      )
                    })}
                  </TableBody>
                </Table>
              </TableContainer>
            </Box>
          </Stack>
        )}
      </AccordionDetails>
    </Accordion>
  )
}
