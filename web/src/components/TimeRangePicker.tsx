import { useState } from 'react'
import { Alert, Box, Button, MenuItem, Stack, TextField, Typography } from '@mui/material'
import { browserTimeZone, customRangeError, fromLocalInput, rangePresets, toLocalInput, type CustomRange } from '../timeRange'

const customValue = 'custom'

type TimeRangePickerProps = {
  preset: string
  custom?: CustomRange
  onPreset: (value: string) => void
  onCustom: (range: CustomRange) => void
}

/**
 * Preset lookbacks plus a custom start and end, entered in the browser's time
 * zone and sent to the API in UTC. Ranges are 1 minute to 365 days.
 */
export function TimeRangePicker({ preset, custom, onPreset, onCustom }: TimeRangePickerProps) {
  const [editing, setEditing] = useState(false)
  const [start, setStart] = useState(() => toLocalInput(custom?.start ?? Date.now() - 24 * 3_600_000))
  const [end, setEnd] = useState(() => toLocalInput(custom?.end ?? Date.now()))
  const draft = { start: fromLocalInput(start) ?? '', end: fromLocalInput(end) ?? '' }
  const error = customRangeError(draft)
  const selectValue = editing || custom ? customValue : preset

  return (
    <Stack sx={{ gap: 1, alignItems: { xs: 'stretch', sm: 'flex-end' } }}>
      <TextField select size="small" label="Range" value={selectValue} sx={{ minWidth: 160 }}
        onChange={(event) => {
          if (event.target.value === customValue) {
            setEditing(true)
            return
          }
          setEditing(false)
          onPreset(event.target.value)
        }}>
        {rangePresets.map((option) => <MenuItem key={option.value} value={option.value}>{option.label}</MenuItem>)}
        <MenuItem value={customValue}>Custom range…</MenuItem>
      </TextField>
      {editing && (
        <Box component="form" aria-label="Custom time range" sx={{ display: 'flex', flexDirection: { xs: 'column', sm: 'row' }, gap: 1, alignItems: { sm: 'flex-start' } }}
          onSubmit={(event) => {
            event.preventDefault()
            if (!error) {
              setEditing(false)
              onCustom(draft)
            }
          }}>
          <TextField size="small" type="datetime-local" label="Start" value={start} onChange={(event) => setStart(event.target.value)} slotProps={{ inputLabel: { shrink: true } }} />
          <TextField size="small" type="datetime-local" label="End" value={end} onChange={(event) => setEnd(event.target.value)} slotProps={{ inputLabel: { shrink: true } }} />
          <Stack direction="row" sx={{ gap: 1 }}>
            <Button type="submit" variant="contained" size="small" disabled={Boolean(error)}>Apply</Button>
            <Button size="small" onClick={() => setEditing(false)}>Cancel</Button>
          </Stack>
        </Box>
      )}
      {editing && error && <Alert severity="warning" sx={{ py: 0 }}>{error}</Alert>}
      {(editing || custom) && <Typography variant="caption" color="text.secondary">Times are in {browserTimeZone()}; queries run in UTC.</Typography>}
    </Stack>
  )
}
