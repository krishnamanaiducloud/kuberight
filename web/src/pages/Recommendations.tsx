import { useState } from 'react'
import { Button } from '@mui/material'
import AutoFixHighOutlined from '@mui/icons-material/AutoFixHighOutlined'
import { DataPage } from '../components/DataPage'
import { GenerateDialog } from '../components/GenerateDialog'
import { RecommendationDrawer } from '../components/RecommendationDrawer'

type RecommendationsProps = { title: string; description: string; columnKeys: readonly string[] }

export function Recommendations({ title, description, columnKeys }: RecommendationsProps) {
  const [generateOpen, setGenerateOpen] = useState(false)
  const [selected, setSelected] = useState<string>()
  return (
    <>
      <DataPage
        title={title}
        description={description}
        endpoint="/api/v1/recommendations"
        columnKeys={columnKeys}
        onRowClick={(item) => setSelected(String(item.id))}
        actions={<Button variant="contained" startIcon={<AutoFixHighOutlined />} onClick={() => setGenerateOpen(true)}>Generate</Button>}
      />
      <GenerateDialog open={generateOpen} onClose={() => setGenerateOpen(false)} onCreated={(record) => { setGenerateOpen(false); setSelected(record.id) }} />
      <RecommendationDrawer id={selected} onClose={() => setSelected(undefined)} />
    </>
  )
}
