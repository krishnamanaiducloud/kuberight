import { Box, Chip, Stack, Table, TableBody, TableCell, TableHead, TableRow, Tooltip, Typography } from '@mui/material'
import type { Backtest as BacktestResult, Exceedance, SpikeProfile, Trends } from '../api'
import { spikeLabel } from '../format'

function share(value: number | undefined) {
  if (value === undefined) return '—'
  if (value === 0) return '0%'
  return value < 0.001 ? '<0.1%' : `${(value * 100).toFixed(1)}%`
}

function Row({ label, current, next }: { label: string; current: Exceedance; next: Exceedance }) {
  return (
    <TableRow>
      <TableCell>{label}</TableCell>
      <TableCell align="right">{share(current.overRequest)}</TableCell>
      <TableCell align="right" sx={{ fontWeight: 750 }}>{share(next.overRequest)}</TableCell>
      <TableCell align="right">{share(current.nearLimit)}</TableCell>
      <TableCell align="right" sx={{ fontWeight: 750 }}>{share(next.nearLimit)}</TableCell>
    </TableRow>
  )
}

/**
 * Replays the observed samples against current and recommended settings.
 * CPU usage can never exceed a CPU limit, so limit pressure is the share of
 * samples at or above 95% of it. Memory above its limit would be OOM-killed.
 */
export function BacktestTable({ backtest }: { backtest: BacktestResult }) {
  const oom = backtest.memoryRecommended.overLimit
  return (
    <Box>
      <Typography variant="subtitle2">Backtest against observed usage</Typography>
      <Typography variant="caption" color="text.secondary" component="p" sx={{ mb: 0.5 }}>
        Share of samples above each request, and at or above 95% of each limit. “—” means the setting is unset.
      </Typography>
      <Table size="small" aria-label="Backtest of current and recommended settings">
        <TableHead>
          <TableRow>
            <TableCell />
            <TableCell align="right">Above request now</TableCell>
            <TableCell align="right">After</TableCell>
            <TableCell align="right">≥95% of limit now</TableCell>
            <TableCell align="right">After</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          <Row label="CPU" current={backtest.cpuCurrent} next={backtest.cpuRecommended} />
          <Row label="Memory" current={backtest.memoryCurrent} next={backtest.memoryRecommended} />
        </TableBody>
      </Table>
      {oom !== undefined && (
        <Typography variant="caption" color={oom > 0 ? 'error.main' : 'text.secondary'}>
          Memory samples above the recommended limit (would be OOM-killed): {share(oom)}
        </Typography>
      )}
    </Box>
  )
}

/** Shows significant usage trends that influenced sizing. */
export function TrendChips({ trends }: { trends?: Trends }) {
  if (!trends) return null
  const chips = [
    trends.cpu.significant && { label: `CPU ${trends.cpu.growthPerDayPercent > 0 ? '+' : ''}${trends.cpu.growthPerDayPercent}%/day (R² ${trends.cpu.rSquared})`, warn: trends.cpu.growthPerDayPercent > 0 },
    trends.memory.significant && { label: `Memory ${trends.memory.growthPerDayPercent > 0 ? '+' : ''}${trends.memory.growthPerDayPercent}%/day (R² ${trends.memory.rSquared})`, warn: trends.memory.growthPerDayPercent > 0 },
  ].filter(Boolean) as Array<{ label: string; warn: boolean }>
  if (chips.length === 0) return null
  return (
    <Stack direction="row" gap={1} flexWrap="wrap">
      {chips.map((chip) => <Chip key={chip.label} size="small" variant="outlined" color={chip.warn ? 'warning' : 'default'} label={`Trend: ${chip.label}`} />)}
    </Stack>
  )
}

const percent = (value: number) => `${(value * 100).toFixed(1)}%`

type BehaviorProps = {
  throttling?: number
  throttlingPeak?: number
  pinned?: boolean
  spikes?: SpikeProfile
  /** Pressure stall (PSI) ratios: CPU wait mean and P95, memory stall P95. */
  cpuPressure?: number
  cpuPressurePeak?: number
  memoryPressurePeak?: number
}

/** CPU throttling, saturation, capped usage, and spike behavior behind a recommendation. */
export function CpuBehaviorChips({ throttling, throttlingPeak, pinned, spikes, cpuPressure, cpuPressurePeak, memoryPressurePeak }: BehaviorProps) {
  const throttled = (throttling ?? 0) >= 0.05 || (throttlingPeak ?? 0) >= 0.25
  const cpuSaturated = (cpuPressure ?? 0) >= 0.05 || (cpuPressurePeak ?? 0) >= 0.10
  const memoryStalled = (memoryPressurePeak ?? 0) >= 0.05
  const hasCpuPressure = cpuPressure !== undefined || cpuPressurePeak !== undefined
  const spike = spikes ? spikeLabel(spikes) : undefined
  if (throttling === undefined && throttlingPeak === undefined && !hasCpuPressure && memoryPressurePeak === undefined && !pinned && !spike) return null
  return (
    <Stack direction="row" gap={1} flexWrap="wrap">
      {(throttling !== undefined || throttlingPeak !== undefined) && (
        <Tooltip title="Share of CFS scheduling periods in which the container was throttled: across the window, and at the 95th percentile of intervals">
          <Chip size="small" variant="outlined" color={throttled ? 'warning' : 'success'} label={`CPU throttled ${percent(throttling ?? 0)} · peak ${percent(throttlingPeak ?? 0)}`} />
        </Tooltip>
      )}
      {hasCpuPressure && (
        <Tooltip title="Share of time runnable tasks waited for a CPU (pressure stall information). Waiting with low usage is saturation, not spare capacity">
          <Chip size="small" variant="outlined" color={cpuSaturated ? 'warning' : 'success'} label={`CPU waiting ${percent(cpuPressure ?? 0)} · peak ${percent(cpuPressurePeak ?? 0)}`} />
        </Tooltip>
      )}
      {memoryPressurePeak !== undefined && (
        <Tooltip title="95th percentile of the share of time tasks stalled on memory reclaim (pressure stall information)">
          <Chip size="small" variant="outlined" color={memoryStalled ? 'warning' : 'success'} label={`Memory stalled peak ${percent(memoryPressurePeak)}`} />
        </Tooltip>
      )}
      {pinned && (
        <Tooltip title="Usage reached the CPU limit, so measured usage is capped and true demand is higher">
          <Chip size="small" variant="outlined" color="warning" label="Pinned at CPU limit" />
        </Tooltip>
      )}
      {spike && <Chip size="small" variant="outlined" color={spikes?.classification === 'rare' ? 'default' : 'warning'} label={spike} />}
    </Stack>
  )
}
