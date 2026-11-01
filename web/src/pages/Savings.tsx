import { useQuery } from '@tanstack/react-query'
import { Box, Card, CardContent, Skeleton, Typography } from '@mui/material'
import { api, type CollectionEnvelope, type SavingsSummary } from '../api'
import { DataPage } from '../components/DataPage'
import { formatCurrency } from '../format'

const columns = ['namespace', 'workload', 'container', 'replicas', 'currentMonthlyCost', 'recommendedMonthlyCost', 'estimatedMonthlySavings', 'reviewStatus'] as const

function SummaryCards() {
  const query = useQuery({
    queryKey: ['resource', 'savings-summary'],
    queryFn: ({ signal }) => api.get<CollectionEnvelope & { summary?: SavingsSummary }>('/api/v1/savings?page=1&pageSize=1', { signal }),
  })
  const summary = query.data?.summary
  if (!query.isLoading && !summary) return null
  const cards = [
    { label: 'Estimated monthly change', value: summary?.estimatedMonthlySavings, note: 'All open and approved recommendations' },
    { label: 'Approved monthly change', value: summary?.approvedMonthlySavings, note: 'Recommendations accepted by a reviewer' },
  ]
  return (
    <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', md: 'repeat(2, 1fr)' }, gap: 2, mb: 2 }}>
      {cards.map((card) => (
        <Card key={card.label}>
          <CardContent>
            <Typography color="text.secondary">{card.label}</Typography>
            {query.isLoading || card.value === undefined ? <Skeleton width="50%" height={56} /> : (
              <Typography variant="h3" sx={{ mt: 1 }}>{formatCurrency(card.value, summary?.currency)}</Typography>
            )}
            <Typography variant="caption" color="text.secondary">{card.note}. Positive values are savings; negative values are increases.</Typography>
          </CardContent>
        </Card>
      ))}
      {summary && <Typography variant="caption" color="text.secondary" sx={{ gridColumn: '1 / -1' }}>Basis: {summary.basis}.{summary.truncated ? ' Only the newest 10,000 recommendations were included.' : ''}</Typography>}
    </Box>
  )
}

export function Savings({ title, description }: { title: string; description: string }) {
  return (
    <DataPage title={title} description={description} endpoint="/api/v1/savings" columnKeys={columns} hideSort>
      <SummaryCards />
    </DataPage>
  )
}
