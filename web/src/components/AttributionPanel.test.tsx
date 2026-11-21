import { fireEvent, render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { vi } from 'vitest'
import { AttributionPanel } from './AttributionPanel'

const subject = { namespace: 'default', kind: 'Deployment', workload: 'netshoot', container: 'netshoot' }

function diagnostics(outcome: string, reason: string, pods: unknown[]) {
  return {
    window: '168h0m0s', step: '5m0s',
    identity: {
      kind: 'Deployment', name: 'netshoot', namespace: 'default', uid: '96d2e881-c4a9-4393-b3bb-34837df30b0b', createdAt: '2026-11-21T04:36:42Z',
      owners: [{ kind: 'ReplicaSet', name: 'netshoot-697fd6796', uid: '5c8f0620-0ef5-418a-8f92-19583f01b377', createdAt: '2026-11-21T04:36:42Z', replicas: 1 }],
      pods: [], resolutionIssues: [],
    },
    report: {
      start: '2026-11-14T05:00:00Z', end: '2026-11-21T05:00:00Z', step: '5m0s', workloadUid: '96d2e881-c4a9-4393-b3bb-34837df30b0b',
      owners: [{ kind: 'ReplicaSet', name: 'netshoot-697fd6796', uid: '5c8f0620-0ef5-418a-8f92-19583f01b377', verified: true, recorded: true }],
      pods, usagePods: 1, attributedPods: outcome === 'attributed' ? 1 : 0, unattributedPods: outcome === 'attributed' ? 0 : 1,
      families: [
        { name: 'kube_pod_owner', purpose: 'links each pod UID to its controller', required: true, series: 1 },
        { name: 'kube_pod_created', purpose: 'pod creation time', required: true, alternative: 'kube_pod_start_time', series: 0 },
        { name: 'kube_pod_start_time', purpose: 'pod start time', required: true, alternative: 'kube_pod_created', series: 1 },
      ],
      missingFamilies: [], coveragePercent: outcome === 'attributed' ? 100 : 0, eligible: outcome === 'attributed', outcome, reason,
      coverage: { complete: outcome === 'attributed', issues: [], discoveredReplicas: 1, revisions: 1, unattributedSteps: 0, ambiguousSteps: 0, cpuReplicaCoverage: 1, memoryReplicaCoverage: 1 },
    },
  }
}

function renderPanel(body: unknown) {
  const fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 200, headers: new Headers(), json: async () => body })
  vi.stubGlobal('fetch', fetchMock)
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><AttributionPanel subject={subject} usageWindow="168h" /></QueryClientProvider>)
  return fetchMock
}

describe('AttributionPanel', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('loads only when opened and shows the UID chain, pods, and substituted families', async () => {
    const fetchMock = renderPanel(diagnostics('attributed', 'every expected replica is attributed to the workload UID with CPU and memory history', [
      { pod: 'netshoot-697fd6796-42dmb', uid: 'a1fd6710-fbcf-491d-a9e4-a5effa0acd91', ownerKind: 'ReplicaSet', owner: 'netshoot-697fd6796', live: true, createdAt: '2026-11-21T04:36:45Z', createdFrom: 'kube_pod_start_time', hasUsage: true, attributed: true, reason: 'attributed' },
    ]))
    expect(fetchMock).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: /Metric attribution/ }))
    expect(await screen.findByText('every expected replica is attributed to the workload UID with CPU and memory history', {}, { timeout: 5000 })).toBeInTheDocument()
    expect(String(fetchMock.mock.calls[0][0])).toContain('/api/v1/attribution?namespace=default&kind=Deployment&workload=netshoot&container=netshoot&window=168h')
    expect(screen.getByText('96d2e881-c4a9-4393-b3bb-34837df30b0b')).toBeInTheDocument()
    expect(screen.getByText('API owner UID')).toBeInTheDocument()
    expect(screen.getByText('Attributed')).toBeInTheDocument()
    expect(screen.getByText('kube_pod_start_time', { selector: 'div' })).toBeInTheDocument()
    // OpenShift does not export kube_pod_created; that is not reported as missing.
    expect(screen.getByText('Not exported; kube_pod_start_time used')).toBeInTheDocument()
    expect(screen.getByText('100.0%')).toBeInTheDocument()
  })

  it('explains why a pod was not attributed', async () => {
    renderPanel(diagnostics('incomplete_attribution', 'workload metric attribution is incomplete: usage exists for pods matching the workload\'s names', [
      { pod: 'netshoot-697fd6796-old01', uid: 'old', ownerKind: 'ReplicaSet', owner: 'netshoot-697fd6796', live: false, createdAt: '2026-11-01T00:00:00Z', createdFrom: 'kube_pod_created', hasUsage: true, attributed: false, reason: 'created at 2026-11-01T00:00:00Z, before the current workload object (2026-11-21T04:36:42Z): a same-name predecessor\'s or an adopted pod' },
    ]))
    fireEvent.click(screen.getByRole('button', { name: /Metric attribution/ }))
    expect(await screen.findByText(/before the current workload object/, {}, { timeout: 5000 })).toBeInTheDocument()
    expect(screen.getAllByText('Workload attribution is incomplete').length).toBeGreaterThan(0)
    expect(screen.getByText('Not possible yet')).toBeInTheDocument()
  })
})
