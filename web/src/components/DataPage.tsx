import { useDeferredValue, useMemo, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Alert, Box, Button, Card, CardContent, Chip, CircularProgress, InputAdornment,
  Checkbox, MenuItem, Pagination, Select, Skeleton, Stack, Table, TableBody, TableCell,
  TableContainer, TableHead, TableRow, TextField, Typography,
} from '@mui/material'
import SearchIcon from '@mui/icons-material/Search'
import RefreshIcon from '@mui/icons-material/Refresh'
import ErrorOutlineIcon from '@mui/icons-material/ErrorOutline'
import { api, ApiError, type ApiRecord, type CollectionEnvelope } from '../api'
import { formatCell } from '../format'

const preferredColumns = ['name', 'cluster', 'namespace', 'kind', 'workload', 'container', 'resource', 'current', 'recommended', 'confidence', 'risk', 'source', 'status', 'health', 'updatedAt', 'createdAt']

const label = (key: string) => key.replace(/([a-z0-9])([A-Z])/g, '$1 $2').replace(/[_-]/g, ' ').replace(/^./, (c) => c.toUpperCase())

function deriveColumns(items: ApiRecord[], requestedColumns?: readonly string[]) {
  const keys = [...new Set(items.flatMap((item) => Object.keys(item)))]
  if (requestedColumns) return requestedColumns.filter((column) => keys.includes(column))
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
  /** Adds a checkbox column keyed by each record's id. */
  selection?: { selected: ReadonlySet<string>; onChange: (next: Set<string>) => void }
}

export function DataPage({ title, description, endpoint, columnKeys, actions, children, onRowClick, hideSort, selection }: DataPageProps) {
  const recommendationPage = endpoint.startsWith('/api/v1/recommendations') || endpoint.startsWith('/api/v1/approvals')
  const [search, setSearch] = useState('')
  const deferredSearch = useDeferredValue(search)
  const [sort, setSort] = useState(recommendationPage ? 'createdAt' : 'name')
  const [page, setPage] = useState(1)
  const pageSize = 25
  const queryPath = `${endpoint}${endpoint.includes('?') ? '&' : '?'}page=${page}&pageSize=${pageSize}&filter=${encodeURIComponent(deferredSearch)}&sort=${encodeURIComponent(sort)}`
  const query = useQuery({ queryKey: ['resource', queryPath], queryFn: ({ signal }) => api.get<CollectionEnvelope>(queryPath, { signal }), retry: (count, error) => !(error instanceof ApiError && [401, 403].includes(error.status)) && count < 2 })
  const data = query.data
  const items = useMemo(() => data?.items ?? [], [data?.items])
  const columns = useMemo(() => deriveColumns(items, columnKeys), [items, columnKeys])
  const total = data?.pagination?.total ?? items.length
  const pages = Math.max(1, Math.ceil(total / (data?.pagination?.pageSize ?? pageSize)))
  const currency = data?.summary?.currency

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
          <Button startIcon={<RefreshIcon />} onClick={() => void query.refetch()} disabled={query.isFetching}>Refresh</Button>
          {actions}
        </Stack>
      </Stack>

      {query.error && (
        <Alert severity={query.error instanceof ApiError && query.error.status === 401 ? 'warning' : 'error'} icon={<ErrorOutlineIcon />} sx={{ mb: 2 }}>
          <strong>{query.error instanceof ApiError && query.error.status === 401 ? 'Authentication required.' : 'Data could not be verified.'}</strong>{' '}
          {query.error instanceof Error ? query.error.message : 'Unknown API error'}
          {query.error instanceof ApiError && query.error.requestId ? ` Request ID: ${query.error.requestId}` : ''}
        </Alert>
      )}
      {data?.status && (data.status.available === false || data.status.reason) && (
        <Alert severity={data.status.available === false ? 'info' : 'warning'} sx={{ mb: 2 }}>
          <strong>{data.status.available === false ? 'Provider unavailable.' : 'Provider warning.'}</strong> {data.status.reason ?? 'This integration has not confirmed any data.'}
        </Alert>
      )}

      {children}

      <Card>
        <CardContent>
          <Stack direction={{ xs: 'column', md: 'row' }} gap={1.5} sx={{ mb: 2 }}>
            <TextField
              size="small"
              label="Search verified records"
              value={search}
              onChange={(event) => { setSearch(event.target.value); setPage(1) }}
              sx={{ minWidth: { md: 320 } }}
              InputProps={{ startAdornment: <InputAdornment position="start"><SearchIcon fontSize="small" /></InputAdornment> }}
            />
            {!hideSort && <Select size="small" value={sort} onChange={(event) => setSort(event.target.value)} aria-label="Sort records">
              {recommendationPage ? <MenuItem value="createdAt">Created</MenuItem> : <MenuItem value="name">Name</MenuItem>}
              <MenuItem value="namespace">Namespace</MenuItem>
              {recommendationPage ? <MenuItem value="workload">Workload</MenuItem> : <MenuItem value="updatedAt">Freshness</MenuItem>}
              {recommendationPage && <MenuItem value="confidence">Confidence</MenuItem>}
              {recommendationPage && <MenuItem value="risk">Risk</MenuItem>}
            </Select>}
            <Box sx={{ flexGrow: 1 }} />
            <Typography variant="body2" color="text.secondary" sx={{ alignSelf: 'center' }}>{query.isFetching ? 'Refreshing…' : `${total} verified record${total === 1 ? '' : 's'}`}</Typography>
          </Stack>

          {query.isLoading ? (
            <Stack gap={1} aria-label="Loading data"><Skeleton height={48} /><Skeleton height={48} /><Skeleton height={48} /></Stack>
          ) : items.length === 0 && !query.error ? (
            <Box sx={{ textAlign: 'center', py: 8 }}>
              <CircularProgress size={28} variant="determinate" value={100} sx={{ color: 'text.secondary', mb: 2 }} />
              <Typography variant="h6">No verified records</Typography>
              <Typography color="text.secondary">KubeRight will not substitute demo data for a missing or empty provider response.</Typography>
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
                        checked={items.length > 0 && items.every((item) => selection.selected.has(String(item.id)))}
                        indeterminate={items.some((item) => selection.selected.has(String(item.id))) && !items.every((item) => selection.selected.has(String(item.id)))}
                        onChange={(event) => {
                          const next = new Set(selection.selected)
                          items.forEach((item) => { if (event.target.checked) next.add(String(item.id)); else next.delete(String(item.id)) })
                          selection.onChange(next)
                        }}
                      />
                    </TableCell>
                  )}
                  {columns.map((column) => <TableCell key={column} sx={{ fontWeight: 800, whiteSpace: 'nowrap' }}>{label(column)}</TableCell>)}
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
                          {['status', 'health', 'confidence', 'risk', 'reviewStatus'].includes(column)
                            ? <Chip size="small" label={formatCell(column, item[column])} color={statusColor(column, item[column])} variant="outlined" />
                            : formatCell(column, item[column], currency)}
                        </TableCell>
                      ))}
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          )}
          {pages > 1 && <Pagination page={page} count={pages} onChange={(_, next) => setPage(next)} sx={{ mt: 2 }} />}
        </CardContent>
      </Card>
    </Box>
  )
}
