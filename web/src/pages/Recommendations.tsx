import { useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Button } from '@mui/material'
import AutoFixHighOutlined from '@mui/icons-material/AutoFixHighOutlined'
import { DataPage } from '../components/DataPage'
import { GenerateDialog } from '../components/GenerateDialog'
import { RecommendationDrawer } from '../components/RecommendationDrawer'
import { EvidenceStatusChip } from '../components/RecommendationEvidence'
import { evidenceRecordFromRow } from '../components/recommendationEvidenceState'

type RecommendationsProps = { title: string; description: string; columnKeys: readonly string[] }

const evidenceColumns = {
  evidence: { label: 'Evidence', render: (item: Record<string, unknown>) => <EvidenceStatusChip record={evidenceRecordFromRow(item)} /> },
}

export function Recommendations({ title, description, columnKeys }: RecommendationsProps) {
  const [generateOpen, setGenerateOpen] = useState(false)
  const [searchParams, setSearchParams] = useSearchParams()
  const selected = searchParams.get('recommendation') ?? undefined
  const selectRecommendation = (id?: string) => {
    setSearchParams((current) => {
      const next = new URLSearchParams(current)
      if (id) next.set('recommendation', id)
      else next.delete('recommendation')
      return next
    })
  }
  return (
    <>
      <DataPage
        title={title}
        description={description}
        endpoint="/api/v1/recommendations"
        columnKeys={[...columnKeys.slice(0, -2), 'evidence', ...columnKeys.slice(-2)]}
        customColumns={evidenceColumns}
        namespaceScope="required"
        onRowClick={(item) => selectRecommendation(String(item.id))}
        actions={<Button variant="contained" startIcon={<AutoFixHighOutlined />} onClick={() => setGenerateOpen(true)}>Generate</Button>}
      />
      <GenerateDialog open={generateOpen} onClose={() => setGenerateOpen(false)} onCreated={(record) => { setGenerateOpen(false); selectRecommendation(record.id) }} />
      <RecommendationDrawer id={selected} onClose={() => selectRecommendation()} />
    </>
  )
}
