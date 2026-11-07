import { Accordion, AccordionDetails, AccordionSummary, Box, MenuItem, Slider, Stack, TextField, ToggleButton, ToggleButtonGroup, Typography } from '@mui/material'
import ExpandMoreIcon from '@mui/icons-material/ExpandMore'
import type { Policy, PolicyProfile } from '../api'

const percentiles = [
  { value: 0.9, label: 'P90' },
  { value: 0.95, label: 'P95' },
  { value: 0.99, label: 'P99' },
  { value: 1, label: 'Peak' },
]

type PolicyTunerProps = {
  profiles: PolicyProfile[]
  profile: string
  policy: Policy
  customized: boolean
  onChange: (profile: string, policy: Policy, customized: boolean) => void
}

function PercentSlider({ label, value, max, onChange }: { label: string; value: number; max: number; onChange: (value: number) => void }) {
  return (
    <Box>
      <Stack direction="row" justifyContent="space-between">
        <Typography variant="body2">{label}</Typography>
        <Typography variant="body2" sx={{ fontWeight: 750 }}>{Math.round(value * 100)}%</Typography>
      </Stack>
      <Slider size="small" min={0} max={max} step={0.05} value={value} onChange={(_, next) => onChange(Math.round((next as number) * 100) / 100)} aria-label={label} />
    </Box>
  )
}

/** Chooses a built-in profile and optionally fine-tunes it. */
export function PolicyTuner({ profiles, profile, policy, customized, onChange }: PolicyTunerProps) {
  const selected = profiles.find((item) => item.name === profile)
  const tune = (patch: Partial<Policy>) => onChange(profile, { ...policy, ...patch }, true)
  return (
    <Stack gap={1.5}>
      <Box>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 0.75 }}>Profile</Typography>
        <ToggleButtonGroup
          exclusive
          size="small"
          fullWidth
          value={profile}
          onChange={(_, next: string | null) => {
            const match = profiles.find((item) => item.name === next)
            if (match) onChange(match.name, match.policy, false)
          }}
          aria-label="Recommendation profile"
        >
          {profiles.map((item) => <ToggleButton key={item.name} value={item.name} sx={{ textTransform: 'capitalize' }}>{item.name}</ToggleButton>)}
        </ToggleButtonGroup>
        {selected && <Typography variant="caption" color="text.secondary" component="p" sx={{ mt: 0.75 }}>{customized ? `Custom policy based on ${selected.name}.` : selected.description}</Typography>}
      </Box>
      <Accordion disableGutters elevation={0} sx={{ bgcolor: 'transparent', border: '1px solid', borderColor: 'divider', borderRadius: 1.5, '&:before': { display: 'none' } }}>
        <AccordionSummary expandIcon={<ExpandMoreIcon />}><Typography variant="body2" sx={{ fontWeight: 700 }}>Fine-tune</Typography></AccordionSummary>
        <AccordionDetails>
          <Stack gap={1.5}>
            <Stack direction={{ xs: 'column', sm: 'row' }} gap={1.5}>
              <TextField select size="small" fullWidth label="CPU sized from" value={policy.cpuPercentile} onChange={(event) => tune({ cpuPercentile: Number(event.target.value) })}>
                {percentiles.map((item) => <MenuItem key={item.value} value={item.value}>{item.label}</MenuItem>)}
              </TextField>
              <TextField select size="small" fullWidth label="Memory sized from" value={policy.memoryPercentile} onChange={(event) => tune({ memoryPercentile: Number(event.target.value) })}>
                {percentiles.map((item) => <MenuItem key={item.value} value={item.value}>{item.label}</MenuItem>)}
              </TextField>
            </Stack>
            <TextField
              select
              size="small"
              label="CPU limit"
              value={policy.cpuLimitStrategy ?? 'adjust'}
              onChange={(event) => tune({ cpuLimitStrategy: event.target.value as Policy['cpuLimitStrategy'] })}
              helperText={(policy.cpuLimitStrategy ?? 'adjust') === 'remove'
                ? 'Requests alone govern CPU share, so throttling cannot occur.'
                : 'Keep existing limits, sized above the largest observed spike.'}
            >
              <MenuItem value="adjust">Adjust to cover spikes</MenuItem>
              <MenuItem value="remove">Remove CPU limits</MenuItem>
            </TextField>
            <PercentSlider label="CPU safety margin" value={policy.cpuSafetyMargin} max={1} onChange={(value) => tune({ cpuSafetyMargin: value })} />
            <PercentSlider label="Memory safety margin" value={policy.memorySafetyMargin} max={1} onChange={(value) => tune({ memorySafetyMargin: value })} />
            <PercentSlider label="Maximum reduction per change" value={policy.maximumReduction} max={0.5} onChange={(value) => tune({ maximumReduction: value })} />
          </Stack>
        </AccordionDetails>
      </Accordion>
    </Stack>
  )
}
