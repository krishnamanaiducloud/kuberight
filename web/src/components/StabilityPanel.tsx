import { Alert, Box, Chip, Stack, Typography } from '@mui/material'
import type { Stability } from '../api'
import { formatDate } from '../format'

// Long evidence chips wrap on narrow screens instead of being cut off.
const wrappingChip = { height: 'auto', maxWidth: '100%', '& .MuiChip-label': { whiteSpace: 'normal', overflowWrap: 'anywhere', py: 0.25 } }

/**
 * Hysteresis, cooldown, oscillation, and confirmation decisions. Increases
 * are never held; this panel explains every withheld reduction.
 */
export function StabilityPanel({ stability }: { stability: Stability }) {
  const held = stability.reductionsHeld.length > 0
  return (
    <Box>
      <Typography variant="subtitle2" sx={{ mb: 0.75 }}>Stability</Typography>
      <Stack direction="row" gap={0.75} flexWrap="wrap" aria-label="Stability controls">
        <Chip size="small" variant="outlined" label={`Minimum change ${Math.round(stability.minimumChangeRatio * 100)}%`} />
        <Chip
          size="small" variant="outlined" color={stability.cooldownActive ? 'warning' : 'default'}
          label={stability.cooldownActive && stability.cooldownEndsAt ? `Cooldown until ${formatDate(stability.cooldownEndsAt)}` : `Cooldown ${stability.cooldownHours}h · not active`}
          sx={wrappingChip}
        />
        <Chip
          size="small" variant="outlined" color={stability.oscillating ? 'warning' : 'default'}
          label={stability.oscillating ? `Oscillating · ${stability.directionChanges} direction changes` : 'No oscillation'}
        />
        <Chip
          size="small" variant="outlined"
          color={stability.consistentObservations < stability.requiredObservations ? 'warning' : 'default'}
          label={`Observations ${Math.min(stability.consistentObservations, stability.requiredObservations)} of ${stability.requiredObservations}`}
        />
      </Stack>
      {held && (
        <Alert severity="warning" variant="outlined" sx={{ mt: 1 }}>
          Reductions held: {stability.reductionsHeld.join(' ')}
        </Alert>
      )}
      {stability.suppressedChanges.length > 0 && (
        <Typography variant="body2" color="text.secondary" sx={{ mt: 1 }}>
          Not changed: {stability.suppressedChanges.join('; ')}.
        </Typography>
      )}
      {stability.lastResourceChangeAt && (
        <Typography variant="caption" color="text.secondary" component="p" sx={{ mt: 0.5 }}>
          Requests or limits last changed {formatDate(stability.lastResourceChangeAt)}.
        </Typography>
      )}
    </Box>
  )
}
