import { render, screen } from '@testing-library/react'
import { EvidenceStatusChip, RecommendationEvidence } from './RecommendationEvidence'
import { recommendationEvidenceState, type EvidenceRecord } from './recommendationEvidenceState'

const now = Date.parse('2026-11-01T12:00:00Z')
const live: EvidenceRecord = {
  source: 'prometheus',
  cluster: 'prod-us',
  createdAt: '2026-11-01T11:05:00Z',
  dataQuality: {
    cpuSampleCount: 1900, memorySampleCount: 1898, minimumRequired: 30, sufficient: true, issues: [],
    replicaCoverage: {
      expectedReplicas: 3, discoveredReplicas: 3, observedReplicas: 3, missingReplicas: 0,
      activeReplicas: 3, terminatedReplicas: 0, cpuObservedReplicas: 3, memoryObservedReplicas: 3,
      cpuReplicaCoverage: 1, memoryReplicaCoverage: 1, cpuTemporalCoverage: 0.99, memoryTemporalCoverage: 0.98,
      cpuImbalanceRatio: 1.2, memoryImbalanceRatio: 1.1, complete: true,
    },
  },
  provenance: {
    workloadUid: '6b4f8c9a', workloadGeneration: 14, workloadResourceVersion: '8821',
    sourceSpecFingerprint: 'a'.repeat(64), queryFingerprint: 'b'.repeat(64), policyFingerprint: 'c'.repeat(64),
    optimizerVersion: '2.0.0', observationStart: '2026-10-25T12:00:00Z', observationEnd: '2026-11-01T11:00:00Z',
    expiresAt: '2026-11-02T11:00:00Z',
  },
}

describe('recommendation evidence', () => {
  it('shows a live workload-bound observation window and audit identifiers', () => {
    render(<RecommendationEvidence record={live} now={now} />)
    expect(screen.getByText('Evidence valid')).toBeInTheDocument()
    expect(screen.getByText(/Generation 14 · prod-us/)).toBeInTheDocument()
    expect(screen.getByText('CPU 1,900 · memory 1,898')).toBeInTheDocument()
    expect(screen.getByText('3 of 3 observed · complete')).toBeInTheDocument()
    expect(screen.getByLabelText('Replica completeness')).toHaveTextContent('All expected replicas covered')
    expect(screen.getByText('Optimizer')).toBeInTheDocument()
    expect(screen.getByLabelText('Technical provenance')).toHaveTextContent('Spec aaaaaaaaaaaa…')
  })

  it('blocks expired Prometheus evidence but keeps caller evidence explicitly manual', () => {
    const expired = recommendationEvidenceState({ ...live, provenance: { ...live.provenance, expiresAt: '2026-11-01T11:59:59Z' } }, now)
    expect(expired).toMatchObject({ kind: 'expired', canApprove: false })
    expect(recommendationEvidenceState({ source: 'request' }, now)).toMatchObject({ kind: 'caller', canApprove: true })
  })

  it('renders a compact expired status for review tables', () => {
    render(<EvidenceStatusChip record={{ ...live, provenance: { ...live.provenance, expiresAt: '2026-11-01T11:59:59Z' } }} now={now} />)
    expect(screen.getByText('Evidence expired').closest('.MuiChip-root')).toHaveClass('MuiChip-colorError')
  })
})
