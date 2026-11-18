import { cpuQuantity, formatBytes, formatCell, formatMillicores, kubectlCommand, memoryQuantity, yamlPatch } from './format'
import type { RecommendationRecord } from './api'

describe('formatting', () => {
  it('renders resource units a reader can compare', () => {
    expect(formatBytes(536870912)).toBe('512 MiB')
    expect(formatBytes(1610612736)).toBe('1.5 GiB')
    expect(formatMillicores(250)).toBe('250m')
    expect(formatMillicores(1500)).toBe('1.5 cores')
    expect(formatMillicores(2000)).toBe('2 cores')
    expect(formatMillicores(1000)).toBe('1 core')
  })

  it('formats cells by column name and marks unset values', () => {
    expect(formatCell('cpuRequestMilli', 800)).toBe('800m')
    expect(formatCell('memoryLimitBytes', 0)).toBe('unset')
    expect(formatCell('estimatedMonthlySavings', 19.71, 'USD')).toContain('19.71')
    expect(formatCell('containers', [{ name: 'api' }, { name: 'proxy' }])).toBe('api, proxy')
    expect(formatCell('namespace', undefined)).toBe('—')
  })

  it('builds Kubernetes quantities', () => {
    expect(cpuQuantity(2000)).toBe('2')
    expect(cpuQuantity(850)).toBe('850m')
    expect(memoryQuantity(512 * 1024 * 1024)).toBe('512Mi')
    expect(memoryQuantity(858993460)).toBe('858993460')
  })

  it('builds a kubectl command without introducing new limits', () => {
    const record = {
      namespace: 'shop', kind: 'Deployment', workload: 'checkout', container: 'api',
      cpuRequestMilli: 800, memoryRequestBytes: 768 * 1024 * 1024, cpuLimitMilli: 0, memoryLimitBytes: 1024 * 1024 * 1024,
    } as RecommendationRecord
    expect(kubectlCommand(record)).toBe('kubectl -n shop set resources deployment/checkout -c api --requests=cpu=800m,memory=768Mi --limits=memory=1024Mi')
    expect(kubectlCommand({ ...record, kind: undefined })).toBeUndefined()
  })
})

describe('yamlPatch', () => {
  const base = { namespace: 'shop', kind: 'Deployment', workload: 'checkout', cpuLimitMilli: 0, memoryLimitBytes: 0 }
  it('groups containers by workload and omits absent limits', () => {
    const yaml = yamlPatch([
      { ...base, container: 'api', cpuRequestMilli: 800, memoryRequestBytes: 768 * 1024 * 1024, cpuLimitMilli: 1600 },
      { ...base, container: 'proxy', cpuRequestMilli: 100, memoryRequestBytes: 64 * 1024 * 1024 },
      { ...base, kind: 'CronJob', workload: 'report', container: 'job', cpuRequestMilli: 2000, memoryRequestBytes: 1024 * 1024 * 1024 },
    ])
    const documents = yaml.split('\n---\n')
    expect(documents).toHaveLength(2)
    expect(documents[0]).toContain('apiVersion: apps/v1\nkind: Deployment')
    expect(documents[0]).toContain('        - name: "api"\n          resources:\n            requests:\n              cpu: "800m"\n              memory: "768Mi"\n            limits:\n              cpu: "1600m"')
    expect(documents[0]).toContain('        - name: "proxy"\n          resources:\n            requests:\n              cpu: "100m"\n              memory: "64Mi"')
    expect(documents[0]).not.toMatch(/proxy[\s\S]*limits/)
    expect(documents[1]).toContain('apiVersion: batch/v1\nkind: CronJob')
    expect(documents[1]).toContain('  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n            - name: "job"')
    expect(documents[1]).toContain('cpu: "2"')
  })

  it('skips records without a supported kind', () => {
    expect(yamlPatch([{ ...base, kind: undefined, container: 'api', cpuRequestMilli: 1, memoryRequestBytes: 1 }])).toBe('')
  })
})
