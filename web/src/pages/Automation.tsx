import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert, AlertTitle, Box, Button, Card, CardContent, Chip, Dialog, DialogActions, DialogContent, DialogContentText, DialogTitle,
  Grid, LinearProgress, Skeleton, Stack, Table, TableBody, TableCell, TableContainer, TableHead, TableRow, Typography, useMediaQuery, useTheme,
} from '@mui/material'
import PlayArrowOutlined from '@mui/icons-material/PlayArrowOutlined'
import { api, type AutomationDecision, type AutomationReport, type AutomationResources } from '../api'
import { RequestAlert } from '../components/RequestAlert'
import { formatBytes, formatMillicores } from '../format'
import { PageHeader } from './PageHeader'

const actionChip: Record<AutomationDecision['action'], { label: string; color: 'success' | 'warning' | 'default' }> = {
  apply: { label: 'Apply', color: 'success' },
  rollback: { label: 'Roll back', color: 'warning' },
  skip: { label: 'Skip', color: 'default' },
}

const outcomeLabel: Record<NonNullable<AutomationDecision['outcome']>, string> = {
  planned: 'Planned', applied: 'Applied', rolled_back: 'Rolled back', deferred: 'Deferred', failed: 'Failed',
}

function quantities(resources: AutomationResources) {
  const limit = (value: number, format: (value: number) => string) => (value > 0 ? format(value) : 'none')
  return `${formatMillicores(resources.cpuRequestMilli)} / ${limit(resources.cpuLimitMilli, formatMillicores)} · ${formatBytes(resources.memoryRequestBytes)} / ${limit(resources.memoryLimitBytes, formatBytes)}`
}

function Tile({ label, value, note }: { label: string; value?: number | string; note: string }) {
  return (
    <Card sx={{ height: '100%' }}>
      <CardContent>
        <Typography variant="body2" color="text.secondary">{label}</Typography>
        {value === undefined ? <Skeleton width="40%" height={48} /> : <Typography variant="h4" component="p" sx={{ fontWeight: 750, mt: 1 }}>{value}</Typography>}
        <Typography variant="caption" color="text.secondary">{note}</Typography>
      </CardContent>
    </Card>
  )
}

/**
 * Continuous rightsizing: what apply mode does with approved recommendations,
 * every reason a change is withheld, and an explicit, confirmed run.
 */
export function Automation({ title, description }: { title: string; description: string }) {
  const queryClient = useQueryClient()
  const [confirming, setConfirming] = useState(false)
  const plan = useQuery({
    queryKey: ['automation'],
    queryFn: ({ signal }) => api.get<AutomationReport>('/api/v1/automation', { signal }),
  })
  const run = useMutation({
    mutationFn: () => api.post<AutomationReport>('/api/v1/automation:run', {}),
    onSuccess: async () => {
      setConfirming(false)
      await queryClient.invalidateQueries({ queryKey: ['automation'] })
      await queryClient.invalidateQueries({ queryKey: ['resource'] })
    },
  })
  const report = run.data ?? plan.data
  const eligible = report?.decisions.filter((decision) => decision.action !== 'skip') ?? []
  const skipped = report?.decisions.filter((decision) => decision.action === 'skip') ?? []
  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <PageHeader eyebrow="CONTINUOUS RIGHTSIZING" title={title} description={description}>
        {report?.enabled && (
          <Button sx={{ mt: 2 }} variant="contained" startIcon={<PlayArrowOutlined />} disabled={run.isPending || eligible.length === 0} onClick={() => setConfirming(true)}>
            Run now
          </Button>
        )}
      </PageHeader>
      {(plan.isFetching || run.isPending) && <LinearProgress sx={{ mb: 2 }} aria-label="Evaluating automation" />}
      {plan.error && <Box sx={{ mb: 2 }}><RequestAlert error={plan.error} resource="the automation plan" onRetry={() => void plan.refetch()} /></Box>}
      {run.error && <Box sx={{ mb: 2 }}><RequestAlert error={run.error} resource="the automation run" /></Box>}
      {report && !report.enabled && (
        <Alert severity="info" variant="outlined" sx={{ mb: 2 }}>
          <AlertTitle>Automation is off</AlertTitle>
          The operation mode is <strong>{report.mode}</strong>, so nothing is changed. This is the plan that <strong>apply</strong> mode would carry out.
          Changes are only made in namespaces labeled <code>kuberight.io/automation=enabled</code>.
        </Alert>
      )}
      {report?.enabled && !report.windowOpen && (
        <Alert severity="warning" variant="outlined" sx={{ mb: 2 }}>Outside the change window {report.window}; eligible changes are deferred.</Alert>
      )}
      <Grid container spacing={2} sx={{ mb: 3 }}>
        <Grid size={{ xs: 6, md: 3 }}><Tile label="Eligible changes" value={report?.eligible} note={`of ${report?.evaluated ?? '—'} approved recommendations`} /></Grid>
        <Grid size={{ xs: 6, md: 3 }}><Tile label="Applied" value={report ? report.applied : undefined} note={run.data ? 'in this run' : 'run to apply'} /></Grid>
        <Grid size={{ xs: 6, md: 3 }}><Tile label="Rolled back" value={report ? report.rolledBack : undefined} note="after a verified regression" /></Grid>
        <Grid size={{ xs: 6, md: 3 }}><Tile label="Per-run limit" value={report?.maxChangesPerRun} note={report?.window ? `window ${report.window}` : 'no change window'} /></Grid>
      </Grid>
      <Stack gap={3}>
        <Decisions title="Changes" empty="No approved recommendation is eligible for an automated change." decisions={eligible} loading={!report} />
        <Decisions title="Withheld" empty="Nothing was withheld." decisions={skipped} loading={!report} />
      </Stack>
      <Dialog open={confirming} onClose={() => setConfirming(false)}>
        <DialogTitle>Apply {Math.min(eligible.length, report?.maxChangesPerRun ?? 0)} changes?</DialogTitle>
        <DialogContent>
          <DialogContentText>
            KubeRight patches the pod template of each workload, which starts a rollout. Each patch is rejected if the workload changed since it was evaluated,
            and every change is audited and rolled back automatically if post-change verification finds a regression.
          </DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirming(false)}>Cancel</Button>
          <Button variant="contained" onClick={() => run.mutate()} disabled={run.isPending}>Apply</Button>
        </DialogActions>
      </Dialog>
    </Box>
  )
}

function Reasons({ decision }: { decision: AutomationDecision }) {
  return (
    <>
      {decision.error && <Typography variant="body2" color="error">{decision.error}</Typography>}
      <Box component="ul" sx={{ m: 0, pl: 2 }}>
        {decision.reasons.map((reason) => <Typography component="li" variant="body2" key={reason} sx={{ overflowWrap: 'anywhere' }}>{reason}</Typography>)}
      </Box>
    </>
  )
}

function ActionCell({ decision }: { decision: AutomationDecision }) {
  return (
    <Stack gap={0.5} alignItems="flex-start">
      <Chip size="small" variant="outlined" color={actionChip[decision.action].color} label={actionChip[decision.action].label} />
      {decision.outcome && <Typography variant="caption">{outcomeLabel[decision.outcome]}</Typography>}
    </Stack>
  )
}

function Decisions({ title, empty, decisions, loading }: { title: string; empty: string; decisions: AutomationDecision[]; loading: boolean }) {
  const theme = useTheme()
  // Phones get stacked entries so the reasons are never scrolled out of view.
  const stacked = useMediaQuery(theme.breakpoints.down('md'))
  if (stacked) {
    return (
      <Card>
        <CardContent>
          <Typography variant="h6" component="h2" sx={{ mb: 1 }}>{title}</Typography>
          {loading && <Skeleton height={80} />}
          {!loading && decisions.length === 0 && <Typography color="text.secondary">{empty}</Typography>}
          {!loading && decisions.length > 0 && (
            <Stack component="ul" aria-label={title} divider={<Box component="li" aria-hidden sx={{ borderTop: 1, borderColor: 'divider', listStyle: 'none' }} />} gap={1.5} sx={{ m: 0, p: 0, listStyle: 'none' }}>
              {decisions.map((decision) => (
                <Box component="li" key={decision.recommendationId}>
                  <Stack direction="row" justifyContent="space-between" alignItems="flex-start" gap={1}>
                    <Box sx={{ minWidth: 0 }}>
                      <Typography variant="body2" sx={{ fontWeight: 600, overflowWrap: 'anywhere' }}>{decision.workload} / {decision.container}</Typography>
                      <Typography variant="caption" color="text.secondary">{decision.namespace} · {decision.kind}</Typography>
                    </Box>
                    <ActionCell decision={decision} />
                  </Stack>
                  <Typography variant="body2" sx={{ mt: 0.75 }}>{quantities(decision.current)}</Typography>
                  <Typography variant="body2" sx={{ fontWeight: 600, mb: 0.5 }}>→ {quantities(decision.desired)}</Typography>
                  <Reasons decision={decision} />
                </Box>
              ))}
            </Stack>
          )}
        </CardContent>
      </Card>
    )
  }
  return (
    <Card>
      <CardContent>
        <Typography variant="h6" component="h2" sx={{ mb: 1 }}>{title}</Typography>
        {loading && <Skeleton height={80} />}
        {!loading && decisions.length === 0 && <Typography color="text.secondary">{empty}</Typography>}
        {!loading && decisions.length > 0 && (
          <TableContainer>
            <Table size="small" aria-label={title}>
              <TableHead>
                <TableRow>
                  <TableCell>Workload</TableCell>
                  <TableCell>Action</TableCell>
                  <TableCell>Current → target (CPU request / limit · memory request / limit)</TableCell>
                  <TableCell>Why</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {decisions.map((decision) => (
                  <TableRow key={decision.recommendationId}>
                    <TableCell sx={{ minWidth: 180 }}>
                      <Typography variant="body2" sx={{ fontWeight: 600, overflowWrap: 'anywhere' }}>{decision.workload} / {decision.container}</Typography>
                      <Typography variant="caption" color="text.secondary">{decision.namespace} · {decision.kind}</Typography>
                    </TableCell>
                    <TableCell><ActionCell decision={decision} /></TableCell>
                    <TableCell sx={{ minWidth: 220 }}>
                      <Typography variant="body2">{quantities(decision.current)}</Typography>
                      <Typography variant="body2" sx={{ fontWeight: 600 }}>→ {quantities(decision.desired)}</Typography>
                    </TableCell>
                    <TableCell sx={{ minWidth: 220 }}><Reasons decision={decision} /></TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableContainer>
        )}
      </CardContent>
    </Card>
  )
}
