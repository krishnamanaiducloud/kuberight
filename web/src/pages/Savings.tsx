import { useQuery } from '@tanstack/react-query'
import { Alert, Box, Card, CardContent, Chip, Skeleton, Typography } from '@mui/material'
import { api, type CollectionEnvelope, type SavingsSummary } from '../api'
import { DataPage } from '../components/DataPage'
import { formatCurrency } from '../format'

const columns = ['stage', 'namespace', 'workload', 'container', 'replicas', 'currentMonthlyCost', 'recommendedMonthlyCost', 'theoreticalMonthlyChange', 'verificationStatus', 'stale'] as const

const stageLabels: Record<string, string> = {
  theoretical: 'Theoretical',
  recommended: 'Recommended',
  approved: 'Approved',
  externally_observed: 'Externally observed',
  verified: 'Verified',
}

const customColumns = {
  stage: {
    label: 'Lifecycle stage',
    render: (item: Record<string, unknown>) => {
      const stage = String(item.stage ?? 'theoretical')
      const color: 'success' | 'primary' | 'info' | 'default' = stage === 'verified' ? 'success' : stage === 'externally_observed' || stage === 'approved' ? 'primary' : stage === 'recommended' ? 'info' : 'default'
      return <Chip size="small" color={color} label={stageLabels[stage] ?? stage} variant={stage === 'theoretical' ? 'outlined' : 'filled'} />
    },
  },
}

function SummaryCards() {
  const query = useQuery({
    queryKey: ['resource', 'savings-summary'],
    queryFn: ({ signal }) => api.get<CollectionEnvelope & { summary?: SavingsSummary }>('/api/v1/savings?page=1&pageSize=1', { signal }),
  })
  const summary = query.data?.summary
  if (!query.isLoading && !summary) return null
  const cards = [
    { label: 'Theoretical', value: summary?.theoreticalMonthlyChange, note: 'Every priced, latest non-rejected model result, including stale advice' },
    { label: 'Recommended', value: summary?.recommendedMonthlyChange, note: 'Fresh advice that passed the approval-safety checks' },
    { label: 'Approved', value: summary?.approvedMonthlyChange, note: 'Fresh recommendations accepted by a reviewer' },
    { label: 'Externally observed', value: summary?.externallyObservedMonthlyChange, note: 'Approved target resources observed on the exact workload identity' },
    { label: 'Verified', value: summary?.verifiedMonthlyChange, note: 'Complete exact-UID post-change evidence with no detected regression' },
  ]
  return (
    <Box sx={{ mb: 2 }}>
      <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', sm: 'repeat(2, 1fr)', xl: 'repeat(5, 1fr)' }, gap: 2 }}>
        {cards.map((card) => (
          <Card key={card.label}>
            <CardContent>
              <Typography color="text.secondary">{card.label}</Typography>
              {query.isLoading || card.value === undefined ? <Skeleton width="50%" height={56} /> : (
                <Typography variant="h4" sx={{ mt: 1 }}>{formatCurrency(card.value, summary?.currency)}</Typography>
              )}
              <Typography variant="caption" color="text.secondary">{card.note}. Positive values are modeled reductions; negative values are increases.</Typography>
            </CardContent>
          </Card>
        ))}
      </Box>
      {summary && (
        <>
          <Alert severity="info" sx={{ mt: 2 }}>
            <strong>Realized infrastructure savings are unavailable.</strong> {summary.realized.reason}
          </Alert>
          <Typography component="p" variant="caption" color="text.secondary" sx={{ mt: 1 }}>
            Basis: {summary.basis}.
            {summary.truncated ? ' Only the newest 10,000 recommendations were included.' : ''}
            {summary.verificationOutcomesTruncated ? ' Verification outcomes were conservatively undercounted because the outcome scan reached its limit.' : ''}
            {' '}Values are a lifecycle funnel, not additive totals.
          </Typography>
        </>
      )}
    </Box>
  )
}

export function Savings({ title, description }: { title: string; description: string }) {
  return (
    <DataPage title={title} description={description} endpoint="/api/v1/savings" columnKeys={columns} customColumns={customColumns} hideSort>
      <SummaryCards />
    </DataPage>
  )
}
