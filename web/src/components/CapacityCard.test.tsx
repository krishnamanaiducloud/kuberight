import { render, screen, within } from '@testing-library/react'
import type { NodeGroupImpact } from '../api'
import { NodeGroupTable } from './CapacityCard'

const group = (overrides: Partial<NodeGroupImpact>): NodeGroupImpact => ({
  name: 'general', provider: 'eks', membership: 'verified', evidence: 'label eks.amazonaws.com/nodegroup',
  nodes: 4, schedulableNodes: 4, instanceTypes: ['m6i.xlarge'], capacityTypes: ['on-demand'], cpuMilli: 15_680, memoryBytes: 60_129_542_144,
  currentNodesNeeded: 4, recommendedNodesNeeded: 3, reclaimableNodes: 1, realization: 'Cluster Autoscaler can remove underutilized nodes.',
  ...overrides,
})

describe('NodeGroupTable', () => {
  it('shows each group with its provider, membership evidence, and node change', () => {
    render(<NodeGroupTable groups={[
      group({}),
      group({ name: 'ocp-worker-b', provider: 'openshift', membership: 'inferred', evidence: 'Machine name ocp-worker-b-q9z4m', instanceTypes: [], capacityTypes: [], reclaimableNodes: 0, currentNodesNeeded: 2, recommendedNodesNeeded: 2 }),
    ]} />)
    const rows = screen.getAllByRole('row')
    expect(within(rows[1]).getByText('general')).toBeInTheDocument()
    expect(within(rows[1]).getByText('EKS node group · m6i.xlarge · on-demand')).toBeInTheDocument()
    expect(within(rows[1]).getByText('Verified').closest('.MuiChip-root')).toHaveClass('MuiChip-colorSuccess')
    expect(within(rows[1]).getByText('4 → 3')).toBeInTheDocument()
    expect(within(rows[2]).getByText('OpenShift MachineSet')).toBeInTheDocument()
    expect(within(rows[2]).getByText('Inferred').closest('.MuiChip-root')).toHaveClass('MuiChip-colorWarning')
    expect(within(rows[2]).getByLabelText('Machine name ocp-worker-b-q9z4m')).toBeInTheDocument()
  })
})

describe('NodeGroupTable size limits', () => {
  it('shows the size range and when the minimum holds nodes', () => {
    render(<NodeGroupTable groups={[
      group({ limit: { minSize: 3, maxSize: 6, source: 'Cluster Autoscaler status kube-system/cluster-autoscaler-status: eks-general-1', membership: 'inferred' }, limitedByMinimum: true, currentNodesNeeded: 4, recommendedNodesNeeded: 1, reclaimableNodes: 1 }),
      group({ name: 'batch', limit: { minSize: 0, maxSize: 2, source: 'MachineAutoscaler openshift-machine-api/batch', membership: 'verified' }, exceedsMaximum: true }),
    ]} />)
    const rows = screen.getAllByRole('row')
    expect(within(rows[1]).getByText('size 3–6 (inferred)')).toHaveAttribute('title', 'Cluster Autoscaler status kube-system/cluster-autoscaler-status: eks-general-1')
    expect(within(rows[1]).getByText('held by minimum size')).toBeInTheDocument()
    expect(within(rows[2]).getByText('size 0–2')).toBeInTheDocument()
    expect(within(rows[2]).getByText('needs more than the maximum size')).toBeInTheDocument()
  })
})
