import { useDeferredValue, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Alert, Autocomplete, MenuItem, Stack, TextField } from '@mui/material'
import { api, type CollectionEnvelope, type Subject, type WorkloadRecord } from '../api'
import { NamespaceSelect } from './NamespaceSelect'
import { RequestAlert } from './RequestAlert'

type WorkloadPickerProps = {
  value: Subject | undefined
  onChange: (subject: Subject | undefined) => void
}

/**
 * Selects a workload container from live inventory. The selection is only
 * ever names returned by the API; free text is used solely as a search filter.
 */
export function WorkloadPicker({ value, onChange }: WorkloadPickerProps) {
  const [namespace, setNamespace] = useState(value?.namespace ?? '')
  const [search, setSearch] = useState('')
  const deferredSearch = useDeferredValue(search)
  const [workload, setWorkload] = useState<WorkloadRecord | null>(null)
  const query = useQuery({
    queryKey: ['workload-picker', namespace, deferredSearch],
    queryFn: ({ signal }) => api.get<CollectionEnvelope>(`/api/v1/workloads?page=1&pageSize=200&sort=name&namespace=${encodeURIComponent(namespace)}&filter=${encodeURIComponent(deferredSearch)}`, { signal }),
    enabled: namespace !== '',
    retry: false,
  })
  const options = (query.data?.items ?? []) as WorkloadRecord[]
  const containers = workload?.containers ?? []

  const selectWorkload = (next: WorkloadRecord | null) => {
    setWorkload(next)
    const first = next?.containers?.[0]?.name
    onChange(next && first ? { namespace: next.namespace, kind: next.kind, workload: next.name, container: first } : undefined)
  }

  return (
    <Stack gap={1.5}>
      {query.error && <RequestAlert error={query.error} resource="workloads" namespaceScoped onRetry={() => void query.refetch()} />}
      {query.data?.status?.available === false && (
        <Alert severity="info">{query.data.status.reason ?? 'Workload inventory is unavailable.'}</Alert>
      )}
      <Stack direction={{ xs: 'column', md: 'row' }} gap={1.5}>
        <NamespaceSelect
          value={namespace}
          onChange={(nextNamespace) => {
            setNamespace(nextNamespace)
            setWorkload(null)
            setSearch('')
            onChange(undefined)
          }}
          helperText={namespace === '' ? 'Select a namespace first' : 'Namespace-scoped inventory'}
          sx={{ flex: 1, minWidth: 180 }}
        />
        <Autocomplete
          sx={{ flex: 2, minWidth: 0 }}
          size="small"
          options={options}
          loading={query.isFetching}
          disabled={namespace === ''}
          value={workload}
          onChange={(_, next) => selectWorkload(next)}
          onInputChange={(_, text, reason) => { if (reason === 'input') setSearch(text) }}
          filterOptions={(items) => items}
          isOptionEqualToValue={(option, selected) => option.id === selected.id}
          getOptionLabel={(option) => `${option.namespace} / ${option.kind} / ${option.name}`}
          renderInput={(params) => <TextField {...params} label="Workload" placeholder="Search namespace or name" />}
          noOptionsText={namespace === '' ? 'Select a namespace first' : query.isError ? 'Inventory could not be loaded' : 'No matching workloads'}
        />
        <TextField
          select
          size="small"
          label="Container"
          sx={{ flex: 1, minWidth: 160 }}
          value={value?.container ?? ''}
          disabled={!workload || containers.length === 0}
          onChange={(event) => workload && onChange({ namespace: workload.namespace, kind: workload.kind, workload: workload.name, container: event.target.value })}
        >
          {containers.map((container) => <MenuItem key={container.name} value={container.name}>{container.name}</MenuItem>)}
        </TextField>
      </Stack>
    </Stack>
  )
}
