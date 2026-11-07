import { useDeferredValue, useMemo, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import {
  Alert, Box, Button, Card, CardContent, Chip, CircularProgress, InputAdornment,
  Checkbox, MenuItem, Pagination, Select, Skeleton, Stack, Table, TableBody, TableCell,
  TableContainer, TableHead, TableRow, TextField, Typography,
} from '@mui/material'
import SearchIcon from '@mui/icons-material/Search'
import RefreshIcon from '@mui/icons-material/Refresh'
import { api, ApiError, type ApiRecord, type CollectionEnvelope } from '../api'
import { formatCell } from '../format'
import { RequestAlert } from './RequestAlert'

const preferredColumns = ['name', 'cluster', 'namespace', 'kind', 'workload', 'container', 'resource', 'current', 'recommended', 'confidence', 'risk', 'source', 'status', 'health', 'updatedAt', 'createdAt']

const label = (key: string) => key.replace(/([a-z0-9])([A-Z])/g, '$1 $2').replace(/[_-]/g, ' ').replace(/^./, (c) => c.toUpperCase())

type CustomColumn = { label: string; render: (item: ApiRecord) => ReactNode }

function deriveColumns(items: ApiRecord[], requestedColumns?: readonly string[], customColumns?: Record<string, CustomColumn>) {
  const keys = [...new Set(items.flatMap((item) => Object.keys(item)))]
  if (requestedColumns) return requestedColumns.filter((column) => keys.includes(column) || Boolean(customColumns?.[column]))
  return keys.sort((a, b) => {
    const ai = preferredColumns.indexOf(a)
    const bi = preferredColumns.indexOf(b)
    if (ai === -1 && bi === -1) return a.localeCompare(b)
    if (ai === -1) return 1
    if (bi === -1) return -1
    return ai - bi
  }).slice(0, 8)
}

function statusColor(column: string, value: unknown): 'default' | 'success' | 'warning' | 'error' {
  const normalized = String(value).toLowerCase()
  if (column === 'risk') {
    if (normalized === 'low') return 'success'
    if (normalized === 'medium') return 'warning'
    if (normalized === 'high') return 'error'
  }
  if (['healthy', 'ready', 'approved', 'high', 'connected'].includes(normalized)) return 'success'
  if (['warning', 'medium', 'pending', 'stale'].includes(normalized)) return 'warning'
  if (['critical', 'failed', 'unhealthy', 'denied', 'low'].includes(normalized)) return 'error'
  return 'default'
}

type DataPageProps = {
  title: string
  description: string
  endpoint: string
  columnKeys?: readonly string[]
  /** Header controls, such as a create button. */
  actions?: ReactNode
  /** Content between the header and the table, such as summary cards. */
  children?: ReactNode
  onRowClick?: (item: ApiRecord) => void
  hideSort?: boolean
  /** Adds a namespace boundary. Required mode waits for an explicit namespace or fleet-wide choice. */
  namespaceScope?: 'optional' | 'required'
  /** Derived cells that are not literal API response fields. */
  customColumns?: Record<string, CustomColumn>
  /** Adds a checkbox column keyed by each record's id. */
  selection?: { selected: ReadonlySet<string>; onChange: (next: Set<string>) => void; isSelectable?: (item: ApiRecord) => boolean }
}

const dns1123Label = /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/

export function DataPage({ title, description, endpoint, columnKeys, actions, children, onRowClick, hideSort, namespaceScope, customColumns, selection }: DataPageProps) {
  const recommendationPage = endpoint.startsWith('/api/v1/recommendations') || endpoint.startsWith('/api/v1/approvals')
  const [searchParams, setSearchParams] = useSearchParams()
  const search = searchParams.get('q') ?? ''
  const deferredSearch = useDeferredValue(search)
  const defaultSort = recommendationPage ? 'createdAt' : 'name'
  const allowedSorts = recommendationPage ? ['createdAt', 'namespace', 'workload', 'confidence', 'risk'] : ['name', 'namespace', 'updatedAt']
  const requestedSort = searchParams.get('sort') ?? defaultSort
  const sort = allowedSorts.includes(requestedSort) ? requestedSort : defaultSort
  const requestedPage = Number(searchParams.get('page') ?? '1')
  const page = Number.isSafeInteger(requestedPage) && requestedPage > 0 ? requestedPage : 1
  const namespace = searchParams.get('namespace') ?? ''
  const allNamespaces = searchParams.get('scope') === 'all'
  const namespaceDraft = searchParams.get('namespaceDraft') ?? namespace
  const invalidNamespace = namespaceDraft !== '' && (namespaceDraft.length > 63 || !dns1123Label.test(namespaceDraft))
  const scopeReady = namespaceScope !== 'required' || namespace !== '' || allNamespaces
  const pageSize = 25
  const queryParameters = new URLSearchParams()
  queryParameters.set('page', String(page))
  queryParameters.set('pageSize', String(pageSize))
  queryParameters.set('filter', deferredSearch)
  queryParameters.set('sort', sort)
  if (namespace) queryParameters.set('namespace', namespace)
  const queryPath = `${endpoint}${endpoint.includes('?') ? '&' : '?'}${queryParameters.toString()}`
  const query = useQuery({
    queryKey: ['resource', queryPath],
    queryFn: ({ signal }) => api.get<CollectionEnvelope>(queryPath, { signal }),
    enabled: scopeReady,
    retry: (count, error) => !(error instanceof ApiError && [401, 403].includes(error.status)) && count < 2,
  })
  const data = query.data
  const items = useMemo(() => data?.items ?? [], [data?.items])
  const columns = useMemo(() => deriveColumns(items, columnKeys, customColumns), [items, columnKeys, customColumns])
  const selectableItems = useMemo(() => selection ? items.filter((item) => selection.isSelectable?.(item) ?? true) : [], [items, selection])
  const total = data?.pagination?.total ?? items.length
  const pages = Math.max(1, Math.ceil(total / (data?.pagination?.pageSize ?? pageSize)))
  const currency = data?.summary?.currency

  const updateSearchParam = (key: string, value?: string) => {
    setSearchParams((current) => {
      const next = new URLSearchParams(current)
      if (value) next.set(key, value)
      else next.delete(key)
      if (key !== 'page') next.delete('page')
      return next
    }, { replace: true })
  }

  const applyNamespace = () => {
    if (invalidNamespace || namespaceDraft === '') return
    setSearchParams((current) => {
      const next = new URLSearchParams(current)
      next.set('namespace', namespaceDraft)
      next.delete('namespaceDraft')
      next.delete('scope')
      next.delete('page')
      return next
    })
  }

  const applyFleetScope = () => {
    setSearchParams((current) => {
      const next = new URLSearchParams(current)
      next.delete('namespace')
      next.delete('namespaceDraft')
      next.set('scope', 'all')
      next.delete('page')
      return next
    })
  }

  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <Stack direction={{ xs: 'column', sm: 'row' }} justifyContent="space-between" alignItems={{ sm: 'flex-end' }} gap={2} sx={{ mb: 3 }}>
        <Box>
          <Typography variant="overline" color="primary.main">LIVE CONTROL PLANE</Typography>
          <Typography variant="h3" component="h1" sx={{ fontSize: { xs: '2.125rem', sm: '3rem' }, overflowWrap: 'anywhere' }}>{title}</Typography>
          <Typography color="text.secondary" sx={{ mt: 1, maxWidth: 760 }}>{description}</Typography>
        </Box>
        <Stack direction="row" alignItems="center" gap={1} flexWrap="wrap">
          {data?.status?.asOf && <Chip size="small" label={`Fresh ${new Date(data.status.asOf).toLocaleString()}`} variant="outlined" />}
          {data?.status?.source && <Chip size="small" label={`Source: ${data.status.source}`} />}
          <Button startIcon={<RefreshIcon />} onClick={() => void query.refetch()} disabled={!scopeReady || query.isFetching}>Refresh</Button>
          {actions}
        </Stack>
      </Stack>

      {namespaceScope && (
        <Card sx={{ mb: 2, bgcolor: 'rgba(102,217,196,.035)' }}>
          <CardContent sx={{ '&:last-child': { pb: 2 } }}>
            <Stack direction={{ xs: 'column', md: 'row' }} gap={1.5} alignItems={{ md: 'center' }}>
              <Box sx={{ flex: 1 }}>
                <Typography variant="subtitle2">Kubernetes namespace scope</Typography>
                <Typography variant="body2" color="text.secondary">Start with the smallest scope your identity can access. Fleet-wide listing is always an explicit choice.</Typography>
              </Box>
              <TextField
                size="small"
                label="Namespace"
                value={namespaceDraft}
                onChange={(event) => {
                  const value = event.target.value.trim().toLowerCase()
                  setSearchParams((current) => {
                    const next = new URLSearchParams(current)
                    if (value === namespace) next.delete('namespaceDraft')
                    else next.set('namespaceDraft', value)
                    return next
                  }, { replace: true })
                }}
                onKeyDown={(event) => { if (event.key === 'Enter') applyNamespace() }}
                error={invalidNamespace}
                helperText={invalidNamespace ? 'Use a DNS-1123 namespace name.' : namespace ? `Scoped to ${namespace}` : allNamespaces ? 'All visible namespaces selected.' : 'No request sent yet.'}
                inputProps={{ maxLength: 63, 'aria-label': 'Kubernetes namespace' }}
                sx={{ minWidth: { md: 260 } }}
              />
              <Stack direction="row" gap={1}>
                <Button variant="contained" onClick={applyNamespace} disabled={invalidNamespace || namespaceDraft === ''}>Apply namespace</Button>
                <Button variant="outlined" onClick={applyFleetScope}>All visible</Button>
              </Stack>
            </Stack>
          </CardContent>
        </Card>
      )}
      {query.error && <Box sx={{ mb: 2 }}><RequestAlert error={query.error} resource={title.toLowerCase()} namespaceScoped={Boolean(namespaceScope)} onRetry={() => void query.refetch()} /></Box>}
      {data?.status && (data.status.available === false || data.status.reason) && (
        <Alert severity={data.status.available === false ? 'info' : 'warning'} sx={{ mb: 2 }}>
          <strong>{data.status.available === false ? 'Provider unavailable.' : 'Provider warning.'}</strong> {data.status.reason ?? 'This integration has not confirmed any data.'}
        </Alert>
      )}

      {children}

      {!scopeReady ? (
        <Alert severity="info">
          <strong>Choose a namespace to begin.</strong> This prevents an accidental cluster-wide list request. Select <strong>All visible</strong> only if this identity has fleet-level access.
        </Alert>
      ) : <Card>
        <CardContent>
          <Stack direction={{ xs: 'column', md: 'row' }} gap={1.5} sx={{ mb: 2 }}>
            <TextField
              size="small"
              label="Search records"
              value={search}
              onChange={(event) => updateSearchParam('q', event.target.value)}
              sx={{ minWidth: { md: 320 } }}
              InputProps={{ startAdornment: <InputAdornment position="start"><SearchIcon fontSize="small" /></InputAdornment> }}
            />
            {!hideSort && <Select size="small" value={sort} onChange={(event) => updateSearchParam('sort', event.target.value)} aria-label="Sort records">
              {recommendationPage ? <MenuItem value="createdAt">Created</MenuItem> : <MenuItem value="name">Name</MenuItem>}
              <MenuItem value="namespace">Namespace</MenuItem>
              {recommendationPage ? <MenuItem value="workload">Workload</MenuItem> : <MenuItem value="updatedAt">Freshness</MenuItem>}
              {recommendationPage && <MenuItem value="confidence">Confidence</MenuItem>}
              {recommendationPage && <MenuItem value="risk">Risk</MenuItem>}
            </Select>}
            <Box sx={{ flexGrow: 1 }} />
            <Typography variant="body2" color="text.secondary" sx={{ alignSelf: 'center' }}>{query.isFetching ? 'Refreshing…' : `${total} record${total === 1 ? '' : 's'}`}</Typography>
          </Stack>

          {query.isLoading ? (
            <Stack gap={1} aria-label="Loading data"><Skeleton height={48} /><Skeleton height={48} /><Skeleton height={48} /></Stack>
          ) : items.length === 0 && !query.error ? (
            <Box sx={{ textAlign: 'center', py: 8 }}>
              <CircularProgress size={28} variant="determinate" value={100} sx={{ color: 'text.secondary', mb: 2 }} />
              <Typography variant="h6">No records</Typography>
              <Typography color="text.secondary">KubeRight will not substitute demo data for a missing or empty provider response.</Typography>
              {(search || namespace) && <Button sx={{ mt: 1 }} onClick={() => {
                setSearchParams((current) => {
                  const next = new URLSearchParams(current)
                  next.delete('q')
                  next.delete('page')
                  return next
                }, { replace: true })
              }}>Clear search</Button>}
            </Box>
          ) : (
            <TableContainer>
              <Table size="small" aria-label={`${title} records`}>
                <TableHead><TableRow>
                  {selection && (
                    <TableCell padding="checkbox">
                      <Checkbox
                        size="small"
                        inputProps={{ 'aria-label': 'Select all on this page' }}
                        checked={selectableItems.length > 0 && selectableItems.every((item) => selection.selected.has(String(item.id)))}
                        indeterminate={selectableItems.some((item) => selection.selected.has(String(item.id))) && !selectableItems.every((item) => selection.selected.has(String(item.id)))}
                        disabled={selectableItems.length === 0}
                        onChange={(event) => {
                          const next = new Set(selection.selected)
                          selectableItems.forEach((item) => { if (event.target.checked) next.add(String(item.id)); else next.delete(String(item.id)) })
                          selection.onChange(next)
                        }}
                      />
                    </TableCell>
                  )}
                  {columns.map((column) => <TableCell key={column} sx={{ fontWeight: 800, whiteSpace: 'nowrap' }}>{customColumns?.[column]?.label ?? label(column)}</TableCell>)}
                </TableRow></TableHead>
                <TableBody>
                  {items.map((item, index) => (
                    <TableRow
                      key={String(item.id ?? item.uid ?? `${page}-${index}`)}
                      hover
                      onClick={onRowClick ? () => onRowClick(item) : undefined}
                      onKeyDown={onRowClick ? (event) => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); onRowClick(item) } } : undefined}
                      tabIndex={onRowClick ? 0 : undefined}
                      sx={onRowClick ? { cursor: 'pointer' } : undefined}
                    >
                      {selection && (
                        <TableCell padding="checkbox" onClick={(event) => event.stopPropagation()}>
                          <Checkbox
                            size="small"
                            inputProps={{ 'aria-label': `Select ${String(item.workload ?? item.name ?? item.id)}` }}
                            checked={selection.selected.has(String(item.id))}
                            disabled={!(selection.isSelectable?.(item) ?? true)}
                            onChange={(event) => {
                              const next = new Set(selection.selected)
                              if (event.target.checked) next.add(String(item.id)); else next.delete(String(item.id))
                              selection.onChange(next)
                            }}
                          />
                        </TableCell>
                      )}
                      {columns.map((column) => (
                        <TableCell key={column} sx={{ maxWidth: 340, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: typeof item[column] === 'object' ? 'normal' : 'nowrap' }}>
                          {customColumns?.[column]?.render(item) ?? (['status', 'health', 'confidence', 'risk', 'reviewStatus'].includes(column)
                            ? <Chip size="small" label={formatCell(column, item[column])} color={statusColor(column, item[column])} variant="outlined" />
                            : formatCell(column, item[column], currency))}
                        </TableCell>
                      ))}
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          )}
          {pages > 1 && <Pagination page={Math.min(page, pages)} count={pages} onChange={(_, next) => updateSearchParam('page', String(next))} sx={{ mt: 2 }} />}
        </CardContent>
      </Card>}
    </Box>
  )
}
