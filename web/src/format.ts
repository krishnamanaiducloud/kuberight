import type { RecommendationRecord, SpikeProfile } from './api'

const binaryUnits = ['B', 'KiB', 'MiB', 'GiB', 'TiB']

export function formatBytes(value: number): string {
  if (!Number.isFinite(value)) return '—'
  let scaled = Math.abs(value)
  let unit = 0
  while (scaled >= 1024 && unit < binaryUnits.length - 1) {
    scaled /= 1024
    unit++
  }
  const digits = scaled >= 100 || unit === 0 ? 0 : 1
  return `${value < 0 ? '-' : ''}${scaled.toFixed(digits)} ${binaryUnits[unit]}`
}

export function formatMillicores(value: number): string {
  if (!Number.isFinite(value)) return '—'
  if (Math.abs(value) >= 1000) {
    const cores = (value / 1000).toFixed(2).replace(/\.?0+$/, '')
    return `${cores} ${cores === '1' ? 'core' : 'cores'}`
  }
  return `${Math.round(value)}m`
}

export function formatCurrency(value: number, currency = 'USD'): string {
  return new Intl.NumberFormat(undefined, { style: 'currency', currency, maximumFractionDigits: 2 }).format(value)
}

export function formatDate(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

export function formatPercent(value: number): string {
  return `${value > 0 ? '+' : ''}${value.toFixed(1)}%`
}

const dateColumns = new Set(['createdAt', 'reviewedAt', 'updatedAt', 'timestamp', 'generatedAt'])

/** Formats a table cell from its column name, so units are always visible. */
export function formatCell(column: string, value: unknown, currency?: string): string {
  if (value === null || value === undefined || value === '') return '—'
  if (typeof value === 'number') {
    if (/Bytes$/.test(column)) return value === 0 ? 'unset' : formatBytes(value)
    if (/Milli$/.test(column)) return value === 0 ? 'unset' : formatMillicores(value)
    if (/(Cost|Savings)$/.test(column)) return formatCurrency(value, currency ?? 'USD')
    if (/Percent$/.test(column)) return formatPercent(value)
    return value.toLocaleString()
  }
  if (typeof value === 'string' && dateColumns.has(column)) return formatDate(value)
  if (typeof value === 'boolean') return value ? 'Yes' : 'No'
  if (Array.isArray(value)) return value.map((item) => (typeof item === 'object' && item && 'name' in item ? String(item.name) : String(item))).join(', ')
  if (typeof value === 'object') return JSON.stringify(value)
  return String(value)
}

/** Kubernetes quantity strings for kubectl. */
export function cpuQuantity(milli: number): string {
  return milli % 1000 === 0 ? String(milli / 1000) : `${milli}m`
}

export function memoryQuantity(bytes: number): string {
  const mebibytes = bytes / (1024 * 1024)
  return Number.isInteger(mebibytes) ? `${mebibytes}Mi` : String(bytes)
}

const kubectlKinds: Record<string, string> = {
  Deployment: 'deployment', StatefulSet: 'statefulset', DaemonSet: 'daemonset',
  ReplicaSet: 'replicaset', Job: 'job', CronJob: 'cronjob',
}

/**
 * Builds a command an operator can run through their own change process.
 * KubeRight never runs it. Limits are only included when the workload
 * already had one, matching the optimizer's no-new-limits rule.
 */
export function kubectlCommand(record: RecommendationRecord): string | undefined {
  const kind = record.kind ? kubectlKinds[record.kind] : undefined
  if (!kind) return undefined
  const requests = `--requests=cpu=${cpuQuantity(record.cpuRequestMilli)},memory=${memoryQuantity(record.memoryRequestBytes)}`
  const limits = [
    record.cpuLimitMilli > 0 ? `cpu=${cpuQuantity(record.cpuLimitMilli)}` : '',
    record.memoryLimitBytes > 0 ? `memory=${memoryQuantity(record.memoryLimitBytes)}` : '',
  ].filter(Boolean).join(',')
  return `kubectl -n ${record.namespace} set resources ${kind}/${record.workload} -c ${record.container} ${requests}${limits ? ` --limits=${limits}` : ''}`
}

const apiVersions: Record<string, string> = {
  Deployment: 'apps/v1', StatefulSet: 'apps/v1', DaemonSet: 'apps/v1', ReplicaSet: 'apps/v1', Job: 'batch/v1', CronJob: 'batch/v1',
}

type PatchSource = Pick<RecommendationRecord, 'namespace' | 'kind' | 'workload' | 'container' | 'cpuRequestMilli' | 'memoryRequestBytes' | 'cpuLimitMilli' | 'memoryLimitBytes'>

/**
 * Builds strategic-merge patches, one YAML document per workload, that can be
 * applied with `kubectl patch --type strategic --patch-file` or listed as
 * kustomize patches. Limits appear only where the workload already had one.
 * Records with an unsupported kind are skipped.
 */
export function yamlPatch(records: PatchSource[]): string {
  const workloads = new Map<string, PatchSource[]>()
  for (const record of records) {
    if (!record.kind || !apiVersions[record.kind]) continue
    const key = `${record.namespace}/${record.kind}/${record.workload}`
    workloads.set(key, [...(workloads.get(key) ?? []), record])
  }
  const documents = [...workloads.values()].map((containers) => {
    const { namespace, kind, workload } = containers[0]
    const cronJob = kind === 'CronJob'
    const indent = cronJob ? '            ' : '        '
    const lines = [
      `# kubectl -n ${namespace} patch ${kubectlKinds[kind!]} ${workload} --type strategic --patch-file <this document>`,
      `apiVersion: ${apiVersions[kind!]}`,
      `kind: ${kind}`,
      'metadata:',
      `  name: ${JSON.stringify(workload)}`,
      `  namespace: ${JSON.stringify(namespace)}`,
      'spec:',
      ...(cronJob ? ['  jobTemplate:', '    spec:', '      template:', '        spec:', '          containers:'] : ['  template:', '    spec:', '      containers:']),
    ]
    for (const container of containers) {
      lines.push(`${indent}- name: ${JSON.stringify(container.container)}`)
      lines.push(`${indent}  resources:`)
      lines.push(`${indent}    requests:`)
      lines.push(`${indent}      cpu: ${JSON.stringify(cpuQuantity(container.cpuRequestMilli))}`)
      lines.push(`${indent}      memory: ${JSON.stringify(memoryQuantity(container.memoryRequestBytes))}`)
      if (container.cpuLimitMilli > 0 || container.memoryLimitBytes > 0) {
        lines.push(`${indent}    limits:`)
        if (container.cpuLimitMilli > 0) lines.push(`${indent}      cpu: ${JSON.stringify(cpuQuantity(container.cpuLimitMilli))}`)
        if (container.memoryLimitBytes > 0) lines.push(`${indent}      memory: ${JSON.stringify(memoryQuantity(container.memoryLimitBytes))}`)
      }
    }
    return lines.join('\n')
  })
  return documents.join('\n---\n') + (documents.length ? '\n' : '')
}

/** Starts a browser download of text content. */
export function downloadText(filename: string, content: string, type = 'application/yaml') {
  const url = URL.createObjectURL(new Blob([content], { type }))
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  link.click()
  URL.revokeObjectURL(url)
}

export function formatRatio(value: number | undefined): string {
  return value === undefined ? '—' : `${Math.round(value * 100)}%`
}

/** A one-line description of a spike profile, or undefined when none. */
export function spikeLabel(spikes: SpikeProfile): string | undefined {
  switch (spikes.classification) {
    case 'recurring':
      return `Spikes recur daily${spikes.recurringHourUtc !== undefined ? ` ~${String(spikes.recurringHourUtc).padStart(2, '0')}:00 UTC` : ''}, ${spikes.peakToMedian}× median`
    case 'frequent':
      return `Frequent spikes: ${spikes.spikesPerDay}/day, ~${spikes.medianDurationMinutes} min, ${spikes.peakToMedian}× median`
    case 'rare':
      return `Rare spikes: ${spikes.count}, up to ${spikes.peakToMedian}× median`
    default:
      return undefined
  }
}
