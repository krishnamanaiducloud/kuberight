import { Box, Tooltip, Typography } from '@mui/material'
import { seriesColors } from '../chartColors'

type UsageMeterProps = {
  label: string
  requested: number
  average?: number
  p95?: number
  format: (value: number) => string
}

/**
 * A meter of usage against what is requested. The track is the request, the
 * fill is average usage, and the tick is P95 usage. When P95 exceeds the
 * request the track ends short of the tick, and the caption says so, so the
 * state never depends on color alone.
 */
export function UsageMeter({ label, requested, average, p95, format }: UsageMeterProps) {
  if (average === undefined || p95 === undefined) {
    return <Typography variant="caption" color="text.secondary">No usage reported</Typography>
  }
  const scale = Math.max(requested, p95, average, 1)
  const percent = (value: number) => `${Math.min(100, (value / scale) * 100)}%`
  const over = requested > 0 && p95 > requested
  const caption = requested > 0 ? `${Math.round((average / requested) * 100)}% of request used on average` : 'No request set'
  return (
    <Tooltip
      title={
        <Box sx={{ p: 0.5 }}>
          <Typography variant="body2" sx={{ fontWeight: 800 }}>{label}</Typography>
          <Typography variant="body2">Requested {format(requested)}</Typography>
          <Typography variant="body2">Average {format(average)}</Typography>
          <Typography variant="body2">P95 {format(p95)}</Typography>
        </Box>
      }
    >
      <Box tabIndex={0} aria-label={`${label}: requested ${format(requested)}, average ${format(average)}, P95 ${format(p95)}`} sx={{ minWidth: 140, outline: 'none' }}>
        <Box sx={{ position: 'relative', height: 10, my: 0.5 }}>
          <Box sx={{ position: 'absolute', inset: 0, width: percent(requested), borderRadius: '0 4px 4px 0', bgcolor: 'rgba(57,135,229,.22)' }} />
          <Box sx={{ position: 'absolute', top: 0, bottom: 0, left: 0, width: percent(average), borderRadius: '0 4px 4px 0', bgcolor: seriesColors[0] }} />
          <Box sx={{ position: 'absolute', top: -3, bottom: -3, left: percent(p95), width: 2, ml: '-1px', bgcolor: 'text.primary', borderRadius: 1 }} />
        </Box>
        <Typography variant="caption" color={over ? 'warning.main' : 'text.secondary'}>{over ? 'P95 exceeds request' : caption}</Typography>
      </Box>
    </Tooltip>
  )
}
