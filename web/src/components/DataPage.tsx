import { useDeferredValue, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Alert, Box, Button, Card, CardContent, Chip, CircularProgress, InputAdornment,
  MenuItem, Pagination, Select, Skeleton, Stack, Table, TableBody, TableCell,
  TableContainer, TableHead, TableRow, TextField, Typography,
} from '@mui/material'
import SearchIcon from '@mui/icons-material/Search'
import RefreshIcon from '@mui/icons-material/Refresh'
import ErrorOutlineIcon from '@mui/icons-material/ErrorOutline'
import { api, ApiError, type ApiRecord, type CollectionEnvelope } from '../api'

const preferredColumns = ['name', 'cluster', 'namespace', 'kind', 'workload', 'container', 'resource', 'current', 'recommended', 'confidence', 'risk', 'source', 'status', 'health', 'updatedAt', 'createdAt']

const label = (key: string) => key.replace(/([a-z0-9])([A-Z])/g, '$1 $2').replace(/[_-]/g, ' ').replace(/^./, (c) => c.toUpperCase())

const display = (value: unknown) => {
  if (value === null || value === undefined || value === '') return '—'
  if (typeof value === 'boolean') return value ? 'Yes' : 'No'
  if (typeof value === 'object') return JSON.stringify(value)
  return String(value)
}

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

export function DataPage({ title, description, endpoint, columnKeys }: { title: string; description: string; endpoint: string; columnKeys?: readonly string[] }) {
  const recommendationPage = endpoint === '/api/v1/recommendations'
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

  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <Stack direction={{ xs: 'column', sm: 'row' }} justifyContent="space-between" alignItems={{ sm: 'flex-end' }} gap={2} sx={{ mb: 3 }}>
        <Box>
          <Typography variant="overline" color="primary.main">LIVE CONTROL PLANE</Typography>
          <Typography variant="h3" component="h1">{title}</Typography>
          <Typography color="text.secondary" sx={{ mt: 1, maxWidth: 760 }}>{description}</Typography>
        </Box>
        <Stack direction="row" alignItems="center" gap={1}>
          {data?.status?.asOf && <Chip size="small" label={`Fresh ${new Date(data.status.asOf).toLocaleString()}`} variant="outlined" />}
          {data?.status?.source && <Chip size="small" label={`Source: ${data.status.source}`} />}
          <Button startIcon={<RefreshIcon />} onClick={() => void query.refetch()} disabled={query.isFetching}>Refresh</Button>
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
            <Select size="small" value={sort} onChange={(event) => setSort(event.target.value)} aria-label="Sort records">
              {recommendationPage ? <MenuItem value="createdAt">Created</MenuItem> : <MenuItem value="name">Name</MenuItem>}
              <MenuItem value="namespace">Namespace</MenuItem>
              {recommendationPage ? <MenuItem value="workload">Workload</MenuItem> : <MenuItem value="updatedAt">Freshness</MenuItem>}
              {recommendationPage && <MenuItem value="confidence">Confidence</MenuItem>}
              {recommendationPage && <MenuItem value="risk">Risk</MenuItem>}
            </Select>
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
                <TableHead><TableRow>{columns.map((column) => <TableCell key={column} sx={{ fontWeight: 800, whiteSpace: 'nowrap' }}>{label(column)}</TableCell>)}</TableRow></TableHead>
                <TableBody>
                  {items.map((item, index) => (
                    <TableRow key={String(item.id ?? item.uid ?? `${page}-${index}`)} hover>
                      {columns.map((column) => (
                        <TableCell key={column} sx={{ maxWidth: 340, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: typeof item[column] === 'object' ? 'normal' : 'nowrap' }}>
                          {['status', 'health', 'confidence', 'risk'].includes(column)
                            ? <Chip size="small" label={display(item[column])} color={statusColor(column, item[column])} variant="outlined" />
                            : display(item[column])}
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
