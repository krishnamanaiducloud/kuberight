import { useState } from 'react'
import { Alert, Box, Card, CardContent, MenuItem, Stack, TextField } from '@mui/material'
import type { Subject } from '../api'
import { UsageCharts } from '../components/UsageCharts'
import { WorkloadPicker } from '../components/WorkloadPicker'
import { usageWindows } from '../usageWindows'
import { PageHeader } from './PageHeader'

export function Utilization({ title, description }: { title: string; description: string }) {
  const [subject, setSubject] = useState<Subject>()
  const [usageWindow, setUsageWindow] = useState('24h')
  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <PageHeader eyebrow="PROMETHEUS USAGE" title={title} description={description} />
      <Card sx={{ mb: 2 }}>
        <CardContent>
          <Stack direction={{ xs: 'column', lg: 'row' }} gap={1.5}>
            <Box sx={{ flex: 1, minWidth: 0 }}><WorkloadPicker value={subject} onChange={setSubject} /></Box>
            <TextField select size="small" label="Window" value={usageWindow} onChange={(event) => setUsageWindow(event.target.value)} sx={{ minWidth: 140 }}>
              {usageWindows.map((option) => <MenuItem key={option.value} value={option.value}>{option.label}</MenuItem>)}
            </TextField>
          </Stack>
        </CardContent>
      </Card>
      {subject
        ? <UsageCharts subject={subject} usageWindow={usageWindow} />
        : <Alert severity="info">Choose a workload container to chart its CPU and memory usage against its current requests and limits.</Alert>}
    </Box>
  )
}
