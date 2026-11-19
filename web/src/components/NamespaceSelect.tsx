import { type ReactNode, useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Autocomplete, TextField, type SxProps, type Theme } from '@mui/material'
import { api, type CollectionEnvelope } from '../api'

const namespacePageSize = 200

async function loadNamespaces(signal: AbortSignal): Promise<string[]> {
  const names = new Set<string>()
  let page = 1

  while (true) {
    const result = await api.get<CollectionEnvelope>(
      `/api/v1/namespaces?page=${page}&pageSize=${namespacePageSize}&sort=name`,
      { signal },
    )
    for (const item of result.items) {
      if (typeof item.name === 'string' && item.name !== '') names.add(item.name)
    }

    const pagination = result.pagination
    if (!pagination || result.items.length === 0 || page * pagination.pageSize >= pagination.total) break
    page += 1
  }

  return [...names].sort((left, right) => left.localeCompare(right))
}

export type NamespaceSelectProps = {
  value: string
  onChange: (namespace: string) => void
  label?: string
  helperText?: ReactNode
  required?: boolean
  disabled?: boolean
  size?: 'small' | 'medium'
  fullWidth?: boolean
  sx?: SxProps<Theme>
}

/** A searchable namespace selector whose values always come from live inventory. */
export function NamespaceSelect({
  value,
  onChange,
  label = 'Namespace',
  helperText,
  required,
  disabled,
  size = 'small',
  fullWidth,
  sx,
}: NamespaceSelectProps) {
  const query = useQuery({
    queryKey: ['namespace-options'],
    queryFn: ({ signal }) => loadNamespaces(signal),
    staleTime: 60_000,
    retry: false,
  })
  const options = useMemo(
    () => value && !query.data?.includes(value) ? [value, ...(query.data ?? [])] : (query.data ?? []),
    [query.data, value],
  )

  return (
    <Autocomplete
      options={options}
      value={value || null}
      onChange={(_, next) => onChange(next ?? '')}
      loading={query.isFetching}
      disabled={disabled}
      size={size}
      fullWidth={fullWidth}
      sx={sx}
      autoHighlight
      clearOnEscape
      noOptionsText={query.isError ? 'Namespaces could not be loaded' : 'No visible namespaces'}
      renderInput={(params) => (
        <TextField
          {...params}
          label={label}
          required={required}
          error={query.isError}
          helperText={query.isError ? 'Unable to load namespace inventory.' : helperText}
        />
      )}
    />
  )
}
