import { useMemo, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { Alert, AlertTitle, Box, Card, CardContent, Chip, LinearProgress, MenuItem, Stack, Table, TableBody, TableCell, TableHead, TableRow, TextField, Tooltip, Typography } from '@mui/material'
import ErrorOutline from '@mui/icons-material/ErrorOutline'
import InfoOutlined from '@mui/icons-material/InfoOutlined'
import WarningAmberOutlined from '@mui/icons-material/WarningAmberOutlined'
import { api, ApiError, type CorrelationEvent, type CorrelationPair, type CorrelationResponse, type Subject, type UsageSeries } from '../api'
import { AssessmentPanel, AssessmentStatusChip } from '../components/AssessmentPanel'
import { LineChart, type ChartMarker, type ChartReference } from '../components/LineChart'
import { WorkloadPicker } from '../components/WorkloadPicker'
import { seriesColors, statusColors } from '../chartColors'
import { formatBytes, formatDate, formatMillicores } from '../format'
import { usageWindows } from '../usageWindows'
import { PageHeader } from './PageHeader'

const seriesLabels: Record<string, string> = {
  cpu: 'CPU', cpuPerReplica: 'CPU per replica', memory: 'Memory', throttling: 'Throttling', replicas: 'Replicas',
  latency: 'Latency', requestRate: 'Request rate', nodePressure: 'Node pressure',
}
const label = (name: string) => seriesLabels[name] ?? name

const eventTone: Record<string, ChartMarker['tone']> = { oom_kill: 'critical', restart: 'warning', rollout: 'neutral', scale_up: 'neutral', scale_down: 'neutral' }
const eventLabel: Record<string, string> = { oom_kill: 'OOM kill', restart: 'Restart', rollout: 'Rollout', scale_up: 'Scale up', scale_down: 'Scale down' }

const findingIcon = {
  critical: <ErrorOutline sx={{ color: statusColors.critical }} aria-hidden />,
  warning: <WarningAmberOutlined sx={{ color: statusColors.warning }} aria-hidden />,
  info: <InfoOutlined color="info" aria-hidden />,
}

const points = (series?: UsageSeries, scale = 1) => series?.points.map((point) => ({ time: Date.parse(point.timestamp), value: point.value * scale })) ?? []
const percent = (value: number) => `${value.toFixed(value < 10 ? 1 : 0)}%`
const seconds = (value: number) => (value < 1 ? `${Math.round(value * 1000)} ms` : `${value.toFixed(2)} s`)
const plain = (value: number) => value.toFixed(value % 1 === 0 ? 0 : 1)
const lagText = (pair: CorrelationPair) => {
  if (!pair.leads) return 'simultaneous'
  const minutes = Math.abs(pair.bestLagSeconds) / 60
  const duration = minutes >= 60 ? `${(minutes / 60).toFixed(1)} h` : `${Math.round(minutes)} min`
  return `${label(pair.leads)} leads by ${duration}`
}

function Panel({ title, caption, children }: { title: string; caption?: ReactNode; children: ReactNode }) {
  return (
    <Card>
      <CardContent>
        <Typography variant="h6" component="h2" sx={{ fontWeight: 750 }}>{title}</Typography>
        {caption && <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>{caption}</Typography>}
        {children}
      </CardContent>
    </Card>
  )
}

/** The 95% interval of a correlation on a -1..1 scale, with r marked. */
function IntervalBar({ pair }: { pair: CorrelationPair }) {
  const position = (value: number) => `${((value + 1) / 2) * 100}%`
  const color = pair.significant ? (pair.direction === 'negative' ? seriesColors[1] : seriesColors[0]) : statusColors.neutral
  return (
    <Tooltip title={`r = ${pair.bestLagR.toFixed(2)}, 95% CI ${pair.lower.toFixed(2)} to ${pair.upper.toFixed(2)}, effective n ${pair.effectiveN}`}>
      <Box role="img" aria-label={`Correlation ${pair.bestLagR.toFixed(2)} between ${pair.lower.toFixed(2)} and ${pair.upper.toFixed(2)}`} sx={{ position: 'relative', height: 14, minWidth: 120, bgcolor: 'rgba(148,188,196,.10)', borderRadius: 1 }}>
        <Box sx={{ position: 'absolute', left: '50%', top: 0, bottom: 0, width: '1px', bgcolor: statusColors.neutral }} />
        <Box sx={{ position: 'absolute', left: position(pair.lower), width: `calc(${position(pair.upper)} - ${position(pair.lower)})`, top: 4, height: 6, bgcolor: color, opacity: 0.45, borderRadius: 1 }} />
        <Box sx={{ position: 'absolute', left: `calc(${position(pair.bestLagR)} - 2px)`, top: 1, width: 4, height: 12, bgcolor: color, borderRadius: 1 }} />
      </Box>
    </Tooltip>
  )
}

function eventMarkers(events: CorrelationEvent[]): ChartMarker[] {
  return events.map((event) => ({
    time: Date.parse(event.timestamp), tone: eventTone[event.kind] ?? 'neutral',
    label: `${eventLabel[event.kind] ?? event.kind}${event.detail ? ` (${event.detail})` : ''} at ${formatDate(event.timestamp)}`,
  }))
}

/**
 * Correlation analysis for one container: time-aligned usage, throttling,
 * latency, traffic, and replica charts with deployment and scaling events,
 * correlation strength and direction with confidence intervals and lead/lag,
 * level shifts, forecast against actual usage, the recommendation's evidence
 * and safety verdict, and before/after comparisons.
 */
export function Correlation({ title, description }: { title: string; description: string }) {
  const [params, setParams] = useSearchParams()
  const subject = useMemo<Subject | undefined>(() => {
    const value = { namespace: params.get('namespace') ?? '', kind: params.get('kind') ?? '', workload: params.get('workload') ?? '', container: params.get('container') ?? '' }
    return value.namespace && value.kind && value.workload && value.container ? value : undefined
  }, [params])
  const [usageWindow, setUsageWindow] = useState(params.get('window') ?? '168h')
  const choose = (next?: Subject) => setParams(next ? { ...next, window: usageWindow } : {})

  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <PageHeader eyebrow="EVIDENCE" title={title} description={description} />
      <Card sx={{ mb: 2 }}>
        <CardContent>
          <Stack direction={{ xs: 'column', lg: 'row' }} gap={1.5}>
            <Box sx={{ flex: 1, minWidth: 0 }}><WorkloadPicker value={subject} onChange={choose} /></Box>
            <TextField select size="small" label="Window" value={usageWindow} onChange={(event) => { setUsageWindow(event.target.value); if (subject) setParams({ ...subject, window: event.target.value }) }} sx={{ minWidth: 140 }}>
              {usageWindows.filter((option) => option.value !== '6h').map((option) => <MenuItem key={option.value} value={option.value}>{option.label}</MenuItem>)}
            </TextField>
          </Stack>
        </CardContent>
      </Card>
      {subject
        ? <CorrelationView subject={subject} usageWindow={usageWindow} />
        : <Alert severity="info">Choose a workload container to see how its signals move together and the evidence behind its recommendation.</Alert>}
    </Box>
  )
}

export function CorrelationView({ subject, usageWindow }: { subject: Subject; usageWindow: string }) {
  const query = useQuery({
    queryKey: ['correlation', subject, usageWindow],
    queryFn: ({ signal }) => api.get<CorrelationResponse>(`/api/v1/correlation?${new URLSearchParams({ ...subject, window: usageWindow })}`, { signal }),
  })
  const data = query.data
  if (query.isLoading) return <LinearProgress aria-label="Loading correlation analysis" />
  if (query.error) return <Alert severity="error">{query.error instanceof ApiError ? query.error.message : 'The correlation analysis could not be loaded.'}</Alert>
  if (!data?.signals || !data.report) return <Alert severity="info">{data?.status.reason ?? 'No signals are available.'}</Alert>

  const { signals, report, extra, current } = data
  const domain: [number, number] = [Date.parse(signals.start), Date.parse(signals.end)]
  const markers = eventMarkers(report.events)
  const shared = { domain, markers, height: 190 }
  const cpuReferences: ChartReference[] = [
    ...(current?.cpuRequestMilli ? [{ label: 'Request', value: current.cpuRequestMilli }] : []),
    ...(current?.cpuLimitMilli ? [{ label: 'Limit', value: current.cpuLimitMilli }] : []),
  ]
  const memoryReferences: ChartReference[] = [
    ...(current?.memoryRequestBytes ? [{ label: 'Request', value: current.memoryRequestBytes }] : []),
    ...(current?.memoryLimitBytes ? [{ label: 'Limit', value: current.memoryLimitBytes }] : []),
  ]
  const perReplica = extra?.cpuTotal && signals.replicas
    ? (() => {
        const replicas = new Map(signals.replicas!.points.map((point) => [point.timestamp, point.value]))
        return extra.cpuTotal!.points.filter((point) => (replicas.get(point.timestamp) ?? 0) >= 1).map((point) => ({ time: Date.parse(point.timestamp), value: point.value / replicas.get(point.timestamp)! }))
      })()
    : []
  const recommendation = data.recommendation
  const assessment = recommendation?.assessment
  const material = report.eventImpacts.filter((impact) => impact.material)
  const forecast = data.forecast

  return (
    <Stack gap={2}>
      <Alert severity="info" variant="outlined">
        <AlertTitle>Association, not causation</AlertTitle>
        {report.caveats.join(' ')}
      </Alert>

      {report.findings.length > 0 && (
        <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', md: 'repeat(2, 1fr)' }, gap: 1.5 }}>
          {report.findings.map((finding) => (
            <Card key={finding.title + finding.detail} sx={{ borderLeft: '3px solid', borderLeftColor: finding.severity === 'info' ? 'info.main' : statusColors[finding.severity] }}>
              <CardContent sx={{ py: 1.5, '&:last-child': { pb: 1.5 } }}>
                <Stack direction="row" gap={1} alignItems="center">
                  {findingIcon[finding.severity]}
                  <Typography sx={{ fontWeight: 750 }}>{finding.title}</Typography>
                </Stack>
                <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>{finding.detail}</Typography>
                <Typography variant="caption" color="text.secondary">{finding.evidence}</Typography>
              </CardContent>
            </Card>
          ))}
        </Box>
      )}

      <Panel title="Correlation strength and direction" caption={`Strongest alignment within ±1 hour, from ${formatDate(report.correlationStart)}. The bar is the 95% confidence interval, widened for serial correlation.`}>
        {report.pairs.length === 0
          ? <Typography color="text.secondary">Not enough overlapping data to correlate. CPU and memory charts are still shown below.</Typography>
          : (
            <Box sx={{ overflowX: 'auto' }}>
              <Table size="small" aria-label="Correlations" sx={{ '& th, & td:nth-of-type(2), & td:nth-of-type(6), & td:nth-of-type(7)': { whiteSpace: 'nowrap' } }}>
                <TableHead>
                  <TableRow><TableCell>Signals</TableCell><TableCell>r (95% CI)</TableCell><TableCell sx={{ minWidth: 140 }}>Interval</TableCell><TableCell>Strength</TableCell><TableCell>Timing</TableCell><TableCell>Spearman</TableCell><TableCell>p</TableCell></TableRow>
                </TableHead>
                <TableBody>
                  {report.pairs.map((pair) => (
                    <TableRow key={pair.x + pair.y}>
                      <TableCell><Tooltip title={pair.summary}><span>{label(pair.x)} ↔ {label(pair.y)}</span></Tooltip></TableCell>
                      <TableCell>{pair.bestLagR.toFixed(2)} ({pair.lower.toFixed(2)} to {pair.upper.toFixed(2)})</TableCell>
                      <TableCell><IntervalBar pair={pair} /></TableCell>
                      <TableCell>{pair.significant ? <Chip size="small" variant="outlined" color={pair.strength === 'strong' ? 'warning' : 'default'} label={`${pair.strength} ${pair.direction}`} /> : <Chip size="small" variant="outlined" label="not reliable" />}</TableCell>
                      <TableCell>{pair.significant ? lagText(pair) : '—'}</TableCell>
                      <TableCell>{pair.spearman.toFixed(2)}</TableCell>
                      <TableCell>{pair.pValue <= 1e-6 ? '< 0.000001' : pair.pValue.toPrecision(2)}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </Box>
          )}
      </Panel>

      <Panel title="Time-aligned signals" caption="Every chart shares one time axis. Vertical markers are rollouts, scaling, restarts, and OOM kills.">
        <Stack gap={2}>
          <LineChart title="CPU" series={[{ name: 'Busiest replica', color: seriesColors[0], points: points(signals.cpu) }, ...(perReplica.length ? [{ name: 'Per replica', color: seriesColors[1], points: perReplica }] : [])]} references={cpuReferences} format={formatMillicores} {...shared} />
          {signals.throttling && <LineChart title="CPU throttling" series={[{ name: 'Throttled', color: seriesColors[0], points: points(signals.throttling, 100) }]} references={[{ label: '5% threshold', value: 5 }]} format={percent} {...shared} />}
          <LineChart title="Memory" series={[{ name: 'Working set', color: seriesColors[0], points: points(signals.memory) }]} references={memoryReferences} format={formatBytes} {...shared} />
          {signals.latency && <LineChart title="Latency" series={[{ name: 'Latency', color: seriesColors[0], points: points(signals.latency) }]} format={seconds} {...shared} />}
          {extra?.requestRate && <LineChart title="Request rate" series={[{ name: 'Requests per second', color: seriesColors[0], points: points(extra.requestRate) }]} format={plain} {...shared} />}
          {signals.replicas && <LineChart title="Replicas" series={[{ name: 'Replicas', color: seriesColors[0], points: points(signals.replicas) }]} format={plain} {...shared} />}
          {extra?.nodePressure && <LineChart title="Node pressure" series={[{ name: 'Under pressure', color: seriesColors[1], points: points(extra.nodePressure) }]} format={plain} {...shared} />}
          {!data.configured?.latency && <Typography variant="caption" color="text.secondary">Set KUBERIGHT_LATENCY_QUERY to add application latency.</Typography>}
          {!data.configured?.requestRate && <Typography variant="caption" color="text.secondary">Set KUBERIGHT_REQUEST_RATE_QUERY to add request rate.</Typography>}
        </Stack>
      </Panel>

      <Panel title="Forecast against actual usage" caption={forecast?.available ? `Rolling-origin backtest: each forecast used only earlier hours and is scored on what followed.` : forecast?.reason}>
        {forecast?.available && forecast.points && forecast.overall && (
          <>
            <LineChart
              title="Forecast against actual CPU"
              series={[
                { name: 'Actual', color: seriesColors[0], points: forecast.points.map((point) => ({ time: Date.parse(point.timestamp), value: point.actual })) },
                { name: 'Forecast', color: seriesColors[1], points: forecast.points.map((point) => ({ time: Date.parse(point.timestamp), value: point.predicted })) },
              ]}
              band={{ color: seriesColors[1], points: forecast.points.map((point) => ({ time: Date.parse(point.timestamp), lower: point.lower, upper: point.upper })) }}
              format={formatMillicores}
              height={220}
            />
            <Stack direction="row" gap={1} flexWrap="wrap" sx={{ mt: 1 }}>
              <Chip size="small" variant="outlined" label={`Mean error ${formatMillicores(forecast.overall.mae)}`} />
              <Chip size="small" variant="outlined" color={forecast.overall.coverage >= (forecast.nominalCoverage ?? 0.9) - 0.1 ? 'default' : 'warning'} label={`${Math.round(forecast.overall.coverage * 100)}% inside the ${Math.round((forecast.nominalCoverage ?? 0.9) * 100)}% interval`} />
              {forecast.overall.skill !== undefined && forecast.overall.skill !== null && <Chip size="small" variant="outlined" label={`${forecast.overall.skill >= 0 ? 'Beats' : 'Trails'} the seasonal baseline by ${Math.abs(Math.round(forecast.overall.skill * 100))}%`} />}
              <Chip size="small" variant="outlined" label={`${forecast.overall.samples} held-out hours`} />
            </Stack>
          </>
        )}
      </Panel>

      <Panel title="Recommendation evidence" caption={recommendation ? `Latest recommendation, ${formatDate(recommendation.createdAt)} (${recommendation.reviewStatus})` : 'No recommendation has been generated for this container yet.'}>
        {recommendation && (
          <Stack gap={2}>
            {assessment ? <AssessmentPanel assessment={assessment} /> : <Alert severity="info">This recommendation predates the assessment; regenerate it for the full evidence.</Alert>}
            {recommendation.warnings.length > 0 && (
              <Alert severity="warning" variant="outlined">
                <AlertTitle>Safety warnings</AlertTitle>
                <Box component="ul" sx={{ m: 0, pl: 2.5 }}>{recommendation.warnings.map((warning) => <li key={warning}><Typography variant="body2">{warning}</Typography></li>)}</Box>
              </Alert>
            )}
          </Stack>
        )}
      </Panel>

      <Panel title="Before and after" caption="Change around each event (an hour before against an hour after a 10-minute settling period), and post-change verification of the approved recommendation.">
        <Stack gap={2}>
          {data.verification && (
            <Box>
              <Stack direction="row" gap={1} alignItems="center" sx={{ mb: 1 }}>
                <Typography variant="subtitle2">Post-change verification</Typography>
                <Chip size="small" color={data.verification.status === 'regressed' ? 'error' : data.verification.status === 'verified' ? 'success' : 'default'} label={data.verification.status.replace('_', ' ')} />
                {assessment && <AssessmentStatusChip status={assessment.status} />}
              </Stack>
              {data.verification.reasons.map((reason) => <Typography key={reason} variant="body2" color="text.secondary">{reason}</Typography>)}
              {data.verification.checks.length > 0 && (
                <Box sx={{ overflowX: 'auto' }}>
                  <Table size="small" aria-label="Post-change verification">
                    <TableHead><TableRow><TableCell>Signal</TableCell><TableCell>Before</TableCell><TableCell>After</TableCell><TableCell>Result</TableCell></TableRow></TableHead>
                    <TableBody>
                      {data.verification.checks.map((check) => (
                        <TableRow key={check.name}>
                          <TableCell>{check.name}</TableCell>
                          <TableCell>{check.before ?? '—'}</TableCell>
                          <TableCell>{check.after ?? '—'}</TableCell>
                          <TableCell>{check.regressed ? <Chip size="small" color="error" label="Regressed" /> : 'OK'}</TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </Box>
              )}
            </Box>
          )}
          {material.length > 0
            ? (
              <Box sx={{ overflowX: 'auto' }}>
                <Table size="small" aria-label="Event impacts">
                  <TableHead><TableRow><TableCell>Event</TableCell><TableCell>Signal</TableCell><TableCell>Before</TableCell><TableCell>After</TableCell><TableCell>Change</TableCell></TableRow></TableHead>
                  <TableBody>
                    {material.map((impact) => (
                      <TableRow key={`${impact.event.timestamp}-${impact.event.kind}-${impact.series}`}>
                        <TableCell>{eventLabel[impact.event.kind] ?? impact.event.kind}, {formatDate(impact.event.timestamp)}</TableCell>
                        <TableCell>{label(impact.series)}</TableCell>
                        <TableCell>{impact.before.toPrecision(3)}</TableCell>
                        <TableCell>{impact.after.toPrecision(3)}</TableCell>
                        <TableCell>{impact.relativeChange >= 0 ? '+' : ''}{Math.round(impact.relativeChange * 100)}%</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </Box>
            )
            : <Typography color="text.secondary">No event changed CPU, memory, latency, or throttling by 25% or more.</Typography>}
          {report.changePoints.length > 0 && (
            <Typography variant="body2" color="text.secondary">
              Level shifts: {report.changePoints.map((change) => `${label(change.series)} ${change.relativeChange >= 0 ? '+' : ''}${Math.round(change.relativeChange * 100)}% at ${formatDate(change.timestamp)}${change.nearestEvent ? ` (near a ${eventLabel[change.nearestEvent.kind] ?? change.nearestEvent.kind})` : ''}`).join('; ')}.
            </Typography>
          )}
        </Stack>
      </Panel>

      {(data.warnings ?? []).length > 0 && <Typography variant="caption" color="text.secondary">{data.warnings!.join(' · ')}</Typography>}
    </Stack>
  )
}
