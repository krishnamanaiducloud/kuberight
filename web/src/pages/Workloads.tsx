import { useNavigate } from 'react-router-dom'
import { DataPage } from '../components/DataPage'

const columns = ['name', 'namespace', 'kind', 'health', 'readyReplicas', 'desiredReplicas', 'cpuRequestMilli', 'memoryRequestBytes', 'containers'] as const

export function Workloads({ title, description }: { title: string; description: string }) {
  const navigate = useNavigate()
  return (
    <DataPage
      title={title}
      description={description}
      endpoint="/api/v1/workloads"
      columnKeys={columns}
      namespaceScope="required"
      onRowClick={(item) => navigate(`/workloads/${encodeURIComponent(String(item.namespace))}/${encodeURIComponent(String(item.kind))}/${encodeURIComponent(String(item.name))}`)}
    />
  )
}
