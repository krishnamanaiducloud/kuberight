import type { ReactElement } from 'react'
import DashboardOutlined from '@mui/icons-material/DashboardOutlined'
import LanOutlined from '@mui/icons-material/LanOutlined'
import FolderOutlined from '@mui/icons-material/FolderOutlined'
import WorkspacesOutlined from '@mui/icons-material/WorkspacesOutlined'
import QueryStatsOutlined from '@mui/icons-material/QueryStatsOutlined'
import MemoryOutlined from '@mui/icons-material/MemoryOutlined'
import SavingsOutlined from '@mui/icons-material/SavingsOutlined'
import TimelineOutlined from '@mui/icons-material/TimelineOutlined'
import AutoGraphOutlined from '@mui/icons-material/AutoGraphOutlined'
import TuneOutlined from '@mui/icons-material/TuneOutlined'
import ApprovalOutlined from '@mui/icons-material/ApprovalOutlined'
import AccountTreeOutlined from '@mui/icons-material/AccountTreeOutlined'
import HistoryOutlined from '@mui/icons-material/HistoryOutlined'
import SettingsOutlined from '@mui/icons-material/SettingsOutlined'
import HealthAndSafetyOutlined from '@mui/icons-material/HealthAndSafetyOutlined'

export type NavItem = {
  label: string
  path: string
  endpoint?: string
  description: string
  icon: ReactElement
  columnKeys?: readonly string[]
}

const cpuRecommendationColumns = ['namespace', 'workload', 'container', 'currentCpuRequestMilli', 'cpuRequestMilli', 'currentCpuLimitMilli', 'cpuLimitMilli', 'confidence', 'risk', 'createdAt'] as const
const memoryRecommendationColumns = ['namespace', 'workload', 'container', 'currentMemoryRequestBytes', 'memoryRequestBytes', 'currentMemoryLimitBytes', 'memoryLimitBytes', 'confidence', 'risk', 'createdAt'] as const

export const navigation: NavItem[] = [
  { label: 'Overview', path: '/', endpoint: '/api/v1/overview', description: 'Verified fleet posture and optimization status', icon: <DashboardOutlined /> },
  { label: 'Clusters', path: '/clusters', endpoint: '/api/v1/clusters', description: 'Registered cluster inventory and scope', icon: <LanOutlined /> },
  { label: 'Namespaces', path: '/namespaces', endpoint: '/api/v1/namespaces', description: 'Namespace inventory confirmed by the Kubernetes API', icon: <FolderOutlined /> },
  { label: 'Workloads', path: '/workloads', endpoint: '/api/v1/workloads', description: 'Deployments, StatefulSets, DaemonSets, ReplicaSets, Jobs, and CronJobs', icon: <WorkspacesOutlined /> },
  { label: 'Utilization', path: '/utilization', endpoint: '/api/v1/metrics/history', description: 'Integration status for historical CPU and memory metrics', icon: <QueryStatsOutlined /> },
  { label: 'CPU recommendations', path: '/recommendations/cpu', endpoint: '/api/v1/recommendations', description: 'Explainable CPU requests and limits', icon: <MemoryOutlined />, columnKeys: cpuRecommendationColumns },
  { label: 'Memory recommendations', path: '/recommendations/memory', endpoint: '/api/v1/recommendations', description: 'Memory sizing with OOM and peak safeguards', icon: <MemoryOutlined />, columnKeys: memoryRecommendationColumns },
  { label: 'Savings', path: '/savings', endpoint: '/api/v1/savings', description: 'OpenCost integration status; no savings are estimated without verified cost data', icon: <SavingsOutlined /> },
  { label: 'History', path: '/history', endpoint: '/api/v1/metrics/history', description: 'Historical metrics integration and source freshness', icon: <TimelineOutlined /> },
  { label: 'Forecasting', path: '/forecasting', endpoint: '/api/v1/forecasts', description: 'Forecast integration status; the standalone ML service is not yet connected', icon: <AutoGraphOutlined /> },
  { label: 'Autoscalers', path: '/autoscalers', endpoint: '/api/v1/autoscalers', description: 'HPA, KEDA, and VPA interaction awareness', icon: <TuneOutlined /> },
  { label: 'Policies', path: '/policies', endpoint: '/api/v1/policies', description: 'Recommendation policy and risk controls', icon: <TuneOutlined /> },
  { label: 'Approvals', path: '/approvals', endpoint: '/api/v1/approvals', description: 'Approval integration status; mutation workflows are not implemented', icon: <ApprovalOutlined /> },
  { label: 'GitOps changes', path: '/gitops', endpoint: '/api/v1/gitops/changes', description: 'GitOps integration status; pull-request generation is not implemented', icon: <AccountTreeOutlined /> },
  { label: 'Audit history', path: '/audit', endpoint: '/api/v1/audit', description: 'Security-sensitive and operational event trail', icon: <HistoryOutlined /> },
  { label: 'Settings', path: '/settings', endpoint: '/api/v1/settings/integrations', description: 'Runtime configuration status for supported and pending integrations', icon: <SettingsOutlined /> },
  { label: 'Connection health', path: '/connections', endpoint: '/api/v1/clusters', description: 'Dependency health kept separate from service health', icon: <HealthAndSafetyOutlined /> },
]
