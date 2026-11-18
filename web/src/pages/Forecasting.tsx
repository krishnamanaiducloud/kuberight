import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { Alert, Box, Button, Card, CardContent, Chip, CircularProgress, MenuItem, Stack, TextField, ToggleButton, ToggleButtonGroup, Typography } from '@mui/material'
import { api, ApiError, type ForecastResponse, type Subject } from '../api'
import { seriesColors } from '../chartColors'
import { LineChart } from '../components/LineChart'
import { WorkloadPicker } from '../components/WorkloadPicker'
import { formatBytes, formatMillicores } from '../format'
import { PageHeader } from './PageHeader'

const horizons = [6, 12, 24, 48, 72, 168]

export function Forecasting({ title, description }: { title: string; description: string }) {
  const [subject, setSubject] = useState<Subject>()
  const [resource, setResource] = useState<'cpu' | 'memory'>('cpu')
  const [horizon, setHorizon] = useState(24)
  const mutation = useMutation({
    mutationFn: () => api.post<ForecastResponse>('/api/v1/forecasts', { ...subject, resource, horizon }),
  })
  const result = mutation.data
  const format = result?.unit === 'bytes' ? formatBytes : formatMillicores
  const forecastPoints = result?.forecast.map((point) => ({ time: Date.parse(point.timestamp), value: point.value })) ?? []
  // Show recent history at three times the horizon so the forecast stays
  // legible; the full series is still sent to the model.
  const recentHistory = result?.history.slice(-Math.max(48, result.forecast.length * 3)) ?? []
  // Start the forecast line at the last observation so the two series join.
  const lastObserved = result?.history.at(-1)
  const forecastSeries = lastObserved
    ? [{ time: Date.parse(lastObserved.timestamp), value: lastObserved.value }, ...forecastPoints]
    : forecastPoints

  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <PageHeader eyebrow="EXPERIMENTAL" title={title} description={description} />
      <Card sx={{ mb: 2 }}>
        <CardContent>
          <Stack gap={1.5}>
            <WorkloadPicker value={subject} onChange={(next) => { setSubject(next); mutation.reset() }} />
            <Stack direction={{ xs: 'column', sm: 'row' }} gap={1.5} alignItems={{ sm: 'center' }}>
              <ToggleButtonGroup size="small" exclusive value={resource} onChange={(_, next) => { if (next) { setResource(next); mutation.reset() } }} aria-label="Resource">
                <ToggleButton value="cpu">CPU</ToggleButton>
                <ToggleButton value="memory">Memory</ToggleButton>
              </ToggleButtonGroup>
              <TextField select size="small" label="Horizon" value={horizon} onChange={(event) => setHorizon(Number(event.target.value))} sx={{ minWidth: 160 }}>
                {horizons.map((value) => <MenuItem key={value} value={value}>{value} hours</MenuItem>)}
              </TextField>
              <Box sx={{ flexGrow: 1 }} />
              <Button variant="contained" disabled={!subject || mutation.isPending} onClick={() => mutation.mutate()} startIcon={mutation.isPending ? <CircularProgress size={16} /> : undefined}>Forecast</Button>
            </Stack>
          </Stack>
        </CardContent>
      </Card>
      {mutation.error && <Alert severity="error" sx={{ mb: 2 }}>{mutation.error instanceof ApiError ? mutation.error.message : 'The forecast could not be produced.'}</Alert>}
      {!result && !mutation.error && <Alert severity="info">Forecasts use hourly Prometheus history and the forecasting service. They are advisory and never change recommendations or workloads.</Alert>}
      {result && (
        <Stack gap={2}>
          <Card>
            <CardContent>
              <Stack direction={{ xs: 'column', sm: 'row' }} justifyContent="space-between" gap={1} sx={{ mb: 2 }}>
                <Typography variant="h6">{result.resource === 'cpu' ? 'CPU' : 'Memory'} forecast</Typography>
                <Stack direction="row" gap={1} flexWrap="wrap">
                  <Chip size="small" variant="outlined" label={`Model: ${result.diagnostics.model}`} />
                  <Chip size="small" variant="outlined" label={`Confidence ${Math.round(result.diagnostics.confidence * 100)}%`} />
                  {result.diagnostics.fallbackUsed && <Chip size="small" variant="outlined" color="warning" label="Fallback model" />}
                </Stack>
              </Stack>
              <LineChart
                title={result.resource === 'cpu' ? 'CPU' : 'Memory'}
                series={[
                  { name: 'Observed', color: seriesColors[0], points: recentHistory.map((point) => ({ time: Date.parse(point.timestamp), value: point.value })) },
                  { name: 'Forecast', color: seriesColors[1], points: forecastSeries },
                ]}
                band={{ color: seriesColors[1], points: result.forecast.map((point) => ({ time: Date.parse(point.timestamp), lower: point.lower, upper: point.upper })) }}
                format={format}
                height={300}
              />
            </CardContent>
          </Card>
          {result.diagnostics.fallbackReasons.length > 0 && (
            <Alert severity="info" variant="outlined">
              <strong>Why a fallback was used:</strong> {result.diagnostics.fallbackReasons.join('; ')}
            </Alert>
          )}
          <Typography variant="caption" color="text.secondary">{result.warning} {Math.round(result.confidenceLevel * 100)}% interval. Model version {result.diagnostics.modelVersion}.</Typography>
        </Stack>
      )}
    </Box>
  )
}
