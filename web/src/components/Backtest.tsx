import { Box, Chip, Stack, Table, TableBody, TableCell, TableHead, TableRow, Typography } from '@mui/material'
import type { Backtest as BacktestResult, Exceedance, Trends } from '../api'

function share(value: number | undefined) {
  if (value === undefined) return '—'
  if (value === 0) return '0%'
  return value < 0.001 ? '<0.1%' : `${(value * 100).toFixed(1)}%`
}

function Row({ label, current, next, limitMeaning }: { label: string; current: Exceedance; next: Exceedance; limitMeaning: string }) {
  const oomRisk = label === 'Memory' && (next.overLimit ?? 0) > 0
  return (
    <TableRow>
      <TableCell>{label}</TableCell>
      <TableCell align="right">{share(current.overRequest)}</TableCell>
      <TableCell align="right" sx={{ fontWeight: 750 }}>{share(next.overRequest)}</TableCell>
      <TableCell align="right">{share(current.overLimit)}</TableCell>
      <TableCell align="right" sx={{ fontWeight: 750, color: oomRisk ? 'error.main' : undefined }} title={limitMeaning}>{share(next.overLimit)}</TableCell>
    </TableRow>
  )
}

/**
 * Replays the observed samples against current and recommended settings.
 * Above a memory limit means an OOM kill; above a CPU limit means throttling.
 */
export function BacktestTable({ backtest }: { backtest: BacktestResult }) {
  return (
    <Box>
      <Typography variant="subtitle2">Backtest against observed usage</Typography>
      <Typography variant="caption" color="text.secondary" component="p" sx={{ mb: 0.5 }}>
        Share of samples above each setting. “—” means the setting is unset.
      </Typography>
      <Table size="small" aria-label="Backtest of current and recommended settings">
        <TableHead>
          <TableRow>
            <TableCell />
            <TableCell align="right">Above request now</TableCell>
            <TableCell align="right">After</TableCell>
            <TableCell align="right">Above limit now</TableCell>
            <TableCell align="right">After</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          <Row label="CPU" current={backtest.cpuCurrent} next={backtest.cpuRecommended} limitMeaning="Time CPU would have been throttled" />
          <Row label="Memory" current={backtest.memoryCurrent} next={backtest.memoryRecommended} limitMeaning="Samples that would have been OOM-killed" />
        </TableBody>
      </Table>
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
