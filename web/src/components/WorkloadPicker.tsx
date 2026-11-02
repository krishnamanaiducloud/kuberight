import { useDeferredValue, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Alert, Autocomplete, MenuItem, Stack, TextField } from '@mui/material'
import { api, type CollectionEnvelope, type Subject, type WorkloadRecord } from '../api'
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
  const namespaceValid = /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(namespace) && namespace.length <= 63
  const query = useQuery({
    queryKey: ['workload-picker', namespace, deferredSearch],
    queryFn: ({ signal }) => api.get<CollectionEnvelope>(`/api/v1/workloads?page=1&pageSize=200&sort=name&namespace=${encodeURIComponent(namespace)}&filter=${encodeURIComponent(deferredSearch)}`, { signal }),
    enabled: namespaceValid,
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
        <TextField
          size="small"
          label="Namespace"
          value={namespace}
          onChange={(event) => {
            setNamespace(event.target.value.trim().toLowerCase())
            setWorkload(null)
            setSearch('')
            onChange(undefined)
          }}
          error={namespace !== '' && !namespaceValid}
          helperText={namespace === '' ? 'Required before inventory is queried' : !namespaceValid ? 'Use a DNS-1123 namespace name' : 'Namespace-scoped inventory'}
          inputProps={{ maxLength: 63 }}
          sx={{ flex: 1, minWidth: 180 }}
        />
        <Autocomplete
          sx={{ flex: 2, minWidth: 0 }}
          size="small"
          options={options}
          loading={query.isFetching}
          disabled={!namespaceValid}
          value={workload}
          onChange={(_, next) => selectWorkload(next)}
          onInputChange={(_, text, reason) => { if (reason === 'input') setSearch(text) }}
          filterOptions={(items) => items}
          isOptionEqualToValue={(option, selected) => option.id === selected.id}
          getOptionLabel={(option) => `${option.namespace} / ${option.kind} / ${option.name}`}
          renderInput={(params) => <TextField {...params} label="Workload" placeholder="Search namespace or name" />}
          noOptionsText={!namespaceValid ? 'Enter a namespace first' : query.isError ? 'Inventory could not be loaded' : 'No matching workloads'}
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
