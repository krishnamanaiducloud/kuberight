import type { ReactNode } from 'react'
import { Alert, AlertTitle, Box, Chip, Stack, Table, TableBody, TableCell, TableHead, TableRow, Tooltip, Typography } from '@mui/material'
import CheckCircleOutline from '@mui/icons-material/CheckCircleOutline'
import ErrorOutline from '@mui/icons-material/ErrorOutline'
import HelpOutline from '@mui/icons-material/HelpOutline'
import TuneOutlined from '@mui/icons-material/TuneOutlined'
import RemoveCircleOutline from '@mui/icons-material/RemoveCircleOutline'
import type { Assessment, AssessmentStatus, CheckStatus } from '../api'
import { statusColors } from '../chartColors'
import { formatBytes, formatMillicores } from '../format'

const statusText: Record<AssessmentStatus, { label: string; severity: 'success' | 'warning' | 'info' | 'error'; detail: string }> = {
  recommend: { label: 'Recommended', severity: 'success', detail: 'Every validation passed. The change can be reviewed.' },
  hold: { label: 'Hold', severity: 'warning', detail: 'Keep the current settings until the failed checks are resolved.' },
  insufficient_data: { label: 'Insufficient data', severity: 'info', detail: 'There is not enough reliable history for a safe change yet.' },
  unsupported: { label: 'Unsupported', severity: 'error', detail: 'Another controller manages these resources, so KubeRight does not size them.' },
  no_change: { label: 'Right-sized', severity: 'success', detail: 'The current requests and limits are within the minimum meaningful change. No action is needed.' },
}

const checkIcon: Record<CheckStatus, ReactNode> = {
  pass: <CheckCircleOutline fontSize="small" color="success" aria-hidden />,
  fail: <ErrorOutline fontSize="small" sx={{ color: statusColors.critical }} aria-hidden />,
  adjusted: <TuneOutlined fontSize="small" sx={{ color: statusColors.warning }} aria-hidden />,
  unknown: <HelpOutline fontSize="small" sx={{ color: statusColors.neutral }} aria-hidden />,
  not_applicable: <RemoveCircleOutline fontSize="small" sx={{ color: statusColors.neutral }} aria-hidden />,
}

const checkLabel: Record<CheckStatus, string> = { pass: 'Pass', fail: 'Fail', adjusted: 'Adjusted', unknown: 'Unknown', not_applicable: 'Not applicable' }

const share = (value?: number) => (value === undefined ? '—' : `${(value * 100).toFixed(value < 0.1 ? 1 : 0)}%`)
const margin = (value: number) => `${Math.round(value * 100)}%`
// Long evidence chips wrap on narrow screens instead of being cut off with an ellipsis.
const wrappingChip = { height: 'auto', maxWidth: '100%', '& .MuiChip-label': { whiteSpace: 'normal', overflowWrap: 'anywhere', py: 0.25 } }

/** A pill that names the verdict; used in tables and headers. */
export function AssessmentStatusChip({ status }: { status: AssessmentStatus }) {
  const text = statusText[status]
  return <Chip size="small" color={text.severity} variant={status === 'recommend' ? 'filled' : 'outlined'} label={text.label} />
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Box>
      <Typography variant="subtitle2" sx={{ mb: 0.75 }}>{title}</Typography>
      {children}
    </Box>
  )
}

/**
 * The verdict and every piece of evidence behind it: quantities, dynamic
 * margins, validation replays, the held-back test, admission and scheduling
 * checks, autoscaler impact, QoS, workload totals, forecast, and validity.
 */
export function AssessmentPanel({ assessment }: { assessment: Assessment }) {
  const text = statusText[assessment.status]
  const savings = assessment.expectedSavings
  const forecast = assessment.forecast
  return (
    <Stack gap={2}>
      <Alert severity={text.severity} variant="outlined">
        <AlertTitle>{text.label} · risk {Math.round(assessment.riskScore * 100)}/100 · valid for {assessment.validFor}</AlertTitle>
        {text.detail}
        {assessment.statusReasons.length > 0 && (
          <Box component="ul" sx={{ m: 0, mt: 0.5, pl: 2.5 }}>
            {assessment.statusReasons.map((reason) => <li key={reason}><Typography variant="body2">{reason}</Typography></li>)}
          </Box>
        )}
      </Alert>

      <Section title="Kubernetes quantities">
        <Box sx={{ overflowX: 'auto' }}>
          <Table size="small" aria-label="Current and recommended quantities" sx={{ '& th, & td': { px: { xs: 0.5, sm: 2 }, fontSize: { xs: '0.8rem', sm: '0.875rem' } } }}>
            <TableHead><TableRow><TableCell /><TableCell>CPU request</TableCell><TableCell>CPU limit</TableCell><TableCell>Memory request</TableCell><TableCell>Memory limit</TableCell></TableRow></TableHead>
            <TableBody>
              {([['Current', assessment.current], ['Recommended', assessment.recommended]] as const).map(([label, values]) => (
                <TableRow key={label}>
                  <TableCell component="th" scope="row">{label}</TableCell>
                  <TableCell><code>{values.cpuRequest}</code></TableCell>
                  <TableCell><code>{values.cpuLimit ?? 'none'}</code></TableCell>
                  <TableCell><code>{values.memoryRequest}</code></TableCell>
                  <TableCell><code>{values.memoryLimit ?? 'none'}</code></TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Box>
        <Stack direction="row" gap={1} flexWrap="wrap" sx={{ mt: 1 }}>
          <Chip size="small" variant="outlined" label={`Saves ${formatMillicores(savings.cpuMilli)} CPU and ${formatBytes(savings.memoryBytes)} memory across replicas`} color={savings.cpuMilli < 0 || savings.memoryBytes < 0 ? 'warning' : 'default'} sx={wrappingChip} />
          <Chip size="small" variant="outlined" label={`QoS ${assessment.qos.current} → ${assessment.qos.recommended}${assessment.qos.protected ? ' (protected)' : ''}`} />
          {assessment.workload && (
            <Tooltip title="Effective pod requests: init containers by their maximum, sidecars and application containers summed, overhead added, times replicas.">
              <Chip size="small" variant="outlined" label={`Workload ${formatMillicores(assessment.workload.totalCurrentCpuMilli)} → ${formatMillicores(assessment.workload.totalRecommendedCpuMilli)} CPU${assessment.workload.approximate ? ' (container only)' : ''}`} sx={wrappingChip} />
            </Tooltip>
          )}
        </Stack>
      </Section>

      <Section title="Safety margins">
        <Stack direction="row" gap={1} flexWrap="wrap">
          {(['cpu', 'memory'] as const).map((resource) => {
            const breakdown = assessment.safetyMargins[resource]
            return (
              <Tooltip key={resource} title={`Base ${margin(breakdown.base)}, variability +${margin(breakdown.variability)}, bursts +${margin(breakdown.burst)}, forecast uncertainty +${margin(breakdown.uncertainty)}, criticality +${margin(breakdown.criticality)}`}>
                <Chip size="small" variant="outlined" label={`${resource === 'cpu' ? 'CPU' : 'Memory'} margin ${margin(breakdown.total)}`} />
              </Tooltip>
            )
          })}
          {forecast && (
            <Chip size="small" variant="outlined" color={forecast.calibrated ? 'default' : 'warning'} sx={wrappingChip}
              label={`${forecast.source === 'ml' ? 'ML' : 'Trend'} forecast ${formatMillicores(forecast.cpuMilli)} CPU${forecast.cpuUpperMilli ? ` (≤ ${formatMillicores(forecast.cpuUpperMilli)})` : ''} over ${forecast.horizon}${forecast.empiricalCoverage !== undefined ? `, ${share(forecast.empiricalCoverage)} held-out coverage` : ''}${forecast.usedForSizing ? ' · used for sizing' : ''}`} />
          )}
        </Stack>
      </Section>

      <Section title="Compatibility checks">
        <Stack gap={0.75}>
          {assessment.checks.map((check) => (
            <Stack key={check.name} direction="row" gap={1} alignItems="flex-start">
              {checkIcon[check.status]}
              <Box sx={{ minWidth: 0 }}>
                <Typography variant="body2" sx={{ fontWeight: 650 }}>{check.name} · {checkLabel[check.status]}</Typography>
                <Typography variant="body2" color="text.secondary" sx={{ overflowWrap: 'anywhere' }}>{check.detail}</Typography>
              </Box>
            </Stack>
          ))}
          {assessment.autoscaling.hpa && <Typography variant="body2" color="text.secondary">HPA: {assessment.autoscaling.hpa.detail}</Typography>}
          {assessment.autoscaling.keda && <Typography variant="body2" color="text.secondary">KEDA: {assessment.autoscaling.keda}</Typography>}
          {assessment.autoscaling.vpa && <Typography variant="body2" color="text.secondary">VPA: {assessment.autoscaling.vpa}</Typography>}
        </Stack>
      </Section>

      <Section title="Backtests">
        <Box sx={{ overflowX: 'auto' }}>
          <Table size="small" aria-label="Validation replays">
            <TableHead><TableRow><TableCell>Window</TableCell><TableCell>Samples</TableCell><TableCell>CPU above request</TableCell><TableCell>Memory above request</TableCell><TableCell>Memory above limit</TableCell><TableCell>Result</TableCell></TableRow></TableHead>
            <TableBody>
              {assessment.validation.map((window) => (
                <TableRow key={window.window}>
                  <TableCell>{window.window}</TableCell>
                  <TableCell>{window.samples.toLocaleString()}</TableCell>
                  <TableCell>{share(window.cpuOverRequest)}</TableCell>
                  <TableCell>{share(window.memoryOverRequest)}</TableCell>
                  <TableCell>{share(window.memoryOverLimit)}</TableCell>
                  <TableCell>{window.passed ? 'Pass' : <Tooltip title={window.detail ?? ''}><span>Fail</span></Tooltip>}</TableCell>
                </TableRow>
              ))}
              <TableRow>
                <TableCell>Held-back {assessment.outOfSample.performed ? `${assessment.outOfSample.testSamples.toLocaleString()} samples` : 'test'}</TableCell>
                <TableCell>{assessment.outOfSample.performed ? `${assessment.outOfSample.trainSamples.toLocaleString()} to size` : '—'}</TableCell>
                <TableCell>{share(assessment.outOfSample.cpuOverRequest)}</TableCell>
                <TableCell>{share(assessment.outOfSample.memoryOverRequest)}</TableCell>
                <TableCell>{share(assessment.outOfSample.memoryOverLimit)}</TableCell>
                <TableCell>{assessment.outOfSample.performed ? (assessment.outOfSample.passed ? 'Pass' : 'Fail') : 'Not run'}</TableCell>
              </TableRow>
            </TableBody>
          </Table>
        </Box>
        <Typography variant="caption" color="text.secondary">{assessment.outOfSample.detail}</Typography>
      </Section>

      <Section title="Why">
        <Box component="ul" sx={{ m: 0, pl: 2.5 }}>
          {assessment.explanation.map((line) => <li key={line}><Typography variant="body2">{line}</Typography></li>)}
        </Box>
      </Section>
    </Stack>
  )
}
