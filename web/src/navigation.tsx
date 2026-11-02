import type { ReactElement } from 'react'
import DashboardOutlined from '@mui/icons-material/DashboardOutlined'
import LanOutlined from '@mui/icons-material/LanOutlined'
import FolderOutlined from '@mui/icons-material/FolderOutlined'
import WorkspacesOutlined from '@mui/icons-material/WorkspacesOutlined'
import QueryStatsOutlined from '@mui/icons-material/QueryStatsOutlined'
import MemoryOutlined from '@mui/icons-material/MemoryOutlined'
import SavingsOutlined from '@mui/icons-material/SavingsOutlined'
import AutoGraphOutlined from '@mui/icons-material/AutoGraphOutlined'
import TuneOutlined from '@mui/icons-material/TuneOutlined'
import ApprovalOutlined from '@mui/icons-material/ApprovalOutlined'
import AccountTreeOutlined from '@mui/icons-material/AccountTreeOutlined'
import HistoryOutlined from '@mui/icons-material/HistoryOutlined'
import SettingsOutlined from '@mui/icons-material/SettingsOutlined'
import SpeedOutlined from '@mui/icons-material/SpeedOutlined'
import LocalFireDepartmentOutlined from '@mui/icons-material/LocalFireDepartmentOutlined'
import { Hotspots } from './pages/Hotspots'
import { Efficiency } from './pages/Efficiency'
import { Workloads } from './pages/Workloads'
import { Approvals } from './pages/Approvals'
import { Forecasting } from './pages/Forecasting'
import { Integrations } from './pages/Integrations'
import { Recommendations } from './pages/Recommendations'
import { Savings } from './pages/Savings'
import { Utilization } from './pages/Utilization'

export type NavItem = {
  label: string
  path: string
  endpoint?: string
  description: string
  icon: ReactElement
  columnKeys?: readonly string[]
  /** A dedicated page; items without one render the generic DataPage. */
  render?: (item: NavItem) => ReactElement
}

const cpuRecommendationColumns = ['namespace', 'workload', 'container', 'currentCpuRequestMilli', 'cpuRequestMilli', 'currentCpuLimitMilli', 'cpuLimitMilli', 'confidence', 'risk', 'reviewStatus', 'createdAt'] as const
const memoryRecommendationColumns = ['namespace', 'workload', 'container', 'currentMemoryRequestBytes', 'memoryRequestBytes', 'currentMemoryLimitBytes', 'memoryLimitBytes', 'confidence', 'risk', 'reviewStatus', 'createdAt'] as const

export const navigation: NavItem[] = [
  { label: 'Overview', path: '/', endpoint: '/api/v1/overview', description: 'Verified fleet posture and optimization status', icon: <DashboardOutlined /> },
  { label: 'Efficiency', path: '/efficiency', endpoint: '/api/v1/efficiency', description: 'Requested versus used CPU and memory per namespace, reclaimable capacity, and containers without requests', icon: <SpeedOutlined />, render: (item) => <Efficiency title={item.label} description={item.description} /> },
  { label: 'CPU hotspots', path: '/hotspots', endpoint: '/api/v1/hotspots', description: 'Containers ranked by CPU throttling, usage capped at the limit, and frequent spikes', icon: <LocalFireDepartmentOutlined />, render: (item) => <Hotspots title={item.label} description={item.description} /> },
  { label: 'Clusters', path: '/clusters', endpoint: '/api/v1/clusters', description: 'Registered cluster inventory and scope', icon: <LanOutlined /> },
  { label: 'Namespaces', path: '/namespaces', endpoint: '/api/v1/namespaces', description: 'Namespace inventory confirmed by the Kubernetes API', icon: <FolderOutlined /> },
  { label: 'Workloads', path: '/workloads', endpoint: '/api/v1/workloads', description: 'Deployments, StatefulSets, DaemonSets, ReplicaSets, Jobs, and CronJobs. Select a workload for its containers, usage, and recommendations.', icon: <WorkspacesOutlined />, render: (item) => <Workloads title={item.label} description={item.description} /> },
  { label: 'Utilization', path: '/utilization', endpoint: '/api/v1/metrics/history', description: 'CPU and memory usage from Prometheus against current requests and limits', icon: <QueryStatsOutlined />, render: (item) => <Utilization title={item.label} description={item.description} /> },
  { label: 'CPU recommendations', path: '/recommendations/cpu', endpoint: '/api/v1/recommendations', description: 'Explainable CPU requests and limits. Select a row for details and review.', icon: <MemoryOutlined />, columnKeys: cpuRecommendationColumns, render: (item) => <Recommendations title={item.label} description={item.description} columnKeys={item.columnKeys!} /> },
  { label: 'Memory recommendations', path: '/recommendations/memory', endpoint: '/api/v1/recommendations', description: 'Memory sizing with OOM and peak safeguards. Select a row for details and review.', icon: <MemoryOutlined />, columnKeys: memoryRecommendationColumns, render: (item) => <Recommendations title={item.label} description={item.description} columnKeys={item.columnKeys!} /> },
  { label: 'Approvals', path: '/approvals', endpoint: '/api/v1/approvals', description: 'Review queue. Approval records a decision; KubeRight never applies changes.', icon: <ApprovalOutlined />, render: (item) => <Approvals title={item.label} description={item.description} /> },
  { label: 'Savings', path: '/savings', endpoint: '/api/v1/savings', description: 'Request-based cost estimates from operator-configured unit prices; not billing data', icon: <SavingsOutlined />, render: (item) => <Savings title={item.label} description={item.description} /> },
  { label: 'Forecasting', path: '/forecasting', endpoint: '/api/v1/forecasts', description: 'Experimental usage forecasts with confidence intervals from the forecasting service', icon: <AutoGraphOutlined />, render: (item) => <Forecasting title={item.label} description={item.description} /> },
  { label: 'Autoscalers', path: '/autoscalers', endpoint: '/api/v1/autoscalers', description: 'HPA, KEDA, and VPA interaction awareness', icon: <TuneOutlined /> },
  { label: 'Policies', path: '/policies', endpoint: '/api/v1/policies', description: 'Recommendation policy and risk controls', icon: <TuneOutlined /> },
  { label: 'GitOps changes', path: '/gitops', endpoint: '/api/v1/gitops/changes', description: 'GitOps integration status; pull-request generation is not implemented', icon: <AccountTreeOutlined /> },
  { label: 'Audit history', path: '/audit', endpoint: '/api/v1/audit', description: 'Security-sensitive and operational event trail', icon: <HistoryOutlined /> },
  { label: 'Integrations', path: '/settings', endpoint: '/api/v1/settings/integrations', description: 'Live health of every configured and pending integration', icon: <SettingsOutlined />, render: (item) => <Integrations title={item.label} description={item.description} /> },
]
