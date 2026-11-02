import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import {
  Alert, Box, Button, CircularProgress, Dialog, DialogActions, DialogContent, DialogTitle, Stack, Tab, Tabs, TextField, Typography,
} from '@mui/material'
import FileDownloadOutlined from '@mui/icons-material/FileDownloadOutlined'
import { api, ApiError, type RecommendationRecord } from '../api'
import { DataPage } from '../components/DataPage'
import { RecommendationDrawer } from '../components/RecommendationDrawer'
import { EvidenceStatusChip } from '../components/RecommendationEvidence'
import { evidenceRecordFromRow, recommendationEvidenceState } from '../components/recommendationEvidenceState'
import { downloadText, yamlPatch } from '../format'

const statuses = [
  { value: 'pending', label: 'Awaiting review' },
  { value: 'approved', label: 'Approved' },
  { value: 'rejected', label: 'Rejected' },
  { value: 'all', label: 'All' },
] as const

const columns = ['namespace', 'workload', 'container', 'cpuRequestMilli', 'memoryRequestBytes', 'risk', 'evidence', 'reviewStatus', 'createdBy', 'reviewedBy', 'createdAt'] as const
const evidenceColumns = {
  evidence: { label: 'Evidence', render: (item: Record<string, unknown>) => <EvidenceStatusChip record={evidenceRecordFromRow(item)} /> },
}

type Outcome = { succeeded: number; failures: string[] }

/** Fetches the newest approved recommendation for every container. */
async function approvedRecommendations(): Promise<RecommendationRecord[]> {
  const records: RecommendationRecord[] = []
  for (let page = 1; page <= 50; page++) {
    const result = await api.get<{ items: RecommendationRecord[]; pagination: { total: number } }>(`/api/v1/recommendations?reviewStatus=approved&latest=true&pageSize=200&page=${page}`)
    records.push(...result.items)
    if (records.length >= result.pagination.total || result.items.length === 0) break
  }
  return records
}

export function Approvals({ title, description }: { title: string; description: string }) {
  const client = useQueryClient()
  const [status, setStatus] = useState<(typeof statuses)[number]['value']>('pending')
  const [selected, setSelected] = useState<string>()
  const [checked, setChecked] = useState<Set<string>>(new Set())
  const [rejecting, setRejecting] = useState(false)
  const [comment, setComment] = useState('')
  const [busy, setBusy] = useState(false)
  const [outcome, setOutcome] = useState<Outcome>()
  const [exportError, setExportError] = useState<string>()

  const review = async (decision: 'approve' | 'reject') => {
    setBusy(true)
    setOutcome(undefined)
    const result: Outcome = { succeeded: 0, failures: [] }
    for (const id of checked) {
      try {
        await api.post(`/api/v1/recommendations/${id}/review`, { decision, comment: comment.trim() })
        result.succeeded++
      } catch (error) {
        result.failures.push(error instanceof ApiError ? error.message : `${id}: request failed`)
      }
    }
    setBusy(false)
    setRejecting(false)
    setComment('')
    setChecked(new Set())
    setOutcome(result)
    void client.invalidateQueries({ queryKey: ['resource'] })
    void client.invalidateQueries({ queryKey: ['overview'] })
  }

  const exportApproved = async () => {
    setExportError(undefined)
    try {
      const records = await approvedRecommendations()
      if (records.length === 0) {
        setExportError('There are no approved recommendations to export.')
        return
      }
      downloadText('kuberight-approved-patches.yaml', yamlPatch(records))
    } catch (error) {
      setExportError(error instanceof ApiError ? error.message : 'Approved recommendations could not be exported.')
    }
  }

  const uniqueFailures = outcome ? [...new Set(outcome.failures)] : []

  return (
    <>
      <DataPage
        key={status}
        title={title}
        description={description}
        endpoint={`/api/v1/approvals?reviewStatus=${status}`}
        columnKeys={columns}
        customColumns={evidenceColumns}
        namespaceScope="required"
        onRowClick={(item) => setSelected(String(item.id))}
        selection={status === 'pending' ? {
          selected: checked,
          onChange: setChecked,
          isSelectable: (item) => recommendationEvidenceState(evidenceRecordFromRow(item)).canApprove,
        } : undefined}
        actions={<Button startIcon={<FileDownloadOutlined />} onClick={() => void exportApproved()}>Export approved</Button>}
      >
        <Box sx={{ mb: 2, borderBottom: 1, borderColor: 'divider' }}>
          <Tabs value={status} onChange={(_, next) => { setStatus(next); setChecked(new Set()) }} variant="scrollable" allowScrollButtonsMobile aria-label="Review status">
            {statuses.map((item) => <Tab key={item.value} value={item.value} label={item.label} />)}
          </Tabs>
        </Box>
        {exportError && <Alert severity="info" sx={{ mb: 2 }} onClose={() => setExportError(undefined)}>{exportError}</Alert>}
        {outcome && (
          <Alert severity={uniqueFailures.length ? 'warning' : 'success'} sx={{ mb: 2 }} onClose={() => setOutcome(undefined)}>
            {outcome.succeeded} recommendation{outcome.succeeded === 1 ? '' : 's'} reviewed.
            {uniqueFailures.length > 0 && ` ${outcome.failures.length} could not be reviewed: ${uniqueFailures.join('; ')}`}
          </Alert>
        )}
        {status === 'pending' && checked.size > 0 && (
          <Stack direction={{ xs: 'column', sm: 'row' }} gap={1} alignItems={{ sm: 'center' }} sx={{ mb: 2, p: 1.5, border: '1px solid', borderColor: 'divider', borderRadius: 2 }}>
            <Typography sx={{ flexGrow: 1 }}>{checked.size} selected</Typography>
            <Button variant="contained" color="success" disabled={busy} onClick={() => void review('approve')} startIcon={busy ? <CircularProgress size={16} /> : undefined}>Approve selected</Button>
            <Button variant="outlined" color="error" disabled={busy} onClick={() => setRejecting(true)}>Reject selected</Button>
            <Button disabled={busy} onClick={() => setChecked(new Set())}>Clear</Button>
          </Stack>
        )}
        {status === 'pending' && <Alert severity="info" variant="outlined" sx={{ mb: 2 }}>Expired or unverifiable Prometheus recommendations cannot be selected for approval. Open a row to inspect its evidence and regenerate it.</Alert>}
      </DataPage>
      <Dialog open={rejecting} onClose={() => !busy && setRejecting(false)} fullWidth maxWidth="sm">
        <DialogTitle>Reject {checked.size} recommendation{checked.size === 1 ? '' : 's'}</DialogTitle>
        <DialogContent>
          <TextField autoFocus fullWidth multiline minRows={3} label="Reason" helperText="Recorded with each rejection in the audit trail" value={comment} onChange={(event) => setComment(event.target.value)} inputProps={{ maxLength: 1024 }} sx={{ mt: 1 }} />
        </DialogContent>
        <DialogActions>
          <Button disabled={busy} onClick={() => setRejecting(false)}>Cancel</Button>
          <Button color="error" variant="contained" disabled={busy || comment.trim() === ''} onClick={() => void review('reject')}>Reject</Button>
        </DialogActions>
      </Dialog>
      <RecommendationDrawer id={selected} onClose={() => setSelected(undefined)} />
    </>
  )
}
