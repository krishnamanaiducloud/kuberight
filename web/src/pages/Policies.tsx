import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert, Box, Button, Card, CardContent, Chip, Dialog, DialogActions, DialogContent, DialogContentText, DialogTitle, FormControlLabel,
  LinearProgress, MenuItem, Stack, Switch, Table, TableBody, TableCell, TableContainer, TableHead, TableRow, TextField, Typography,
} from '@mui/material'
import AddOutlined from '@mui/icons-material/AddOutlined'
import {
  api, type Policy, type PolicyLevel, type PolicyListItem, type PolicyProfile, type PolicyRecord, type PolicyResolution, type PolicyScope, type Subject,
} from '../api'
import { NamespaceSelect } from '../components/NamespaceSelect'
import { PolicyTuner } from '../components/PolicyTuner'
import { RequestAlert } from '../components/RequestAlert'
import { WorkloadPicker } from '../components/WorkloadPicker'
import { formatDate, scopeLabel } from '../format'
import { PageHeader } from './PageHeader'

const kinds = ['Deployment', 'StatefulSet', 'DaemonSet', 'ReplicaSet', 'Job', 'CronJob']
const levels: { value: PolicyLevel; label: string }[] = [
  { value: 'global', label: 'Every workload' },
  { value: 'namespace', label: 'A namespace' },
  { value: 'workload', label: 'A workload' },
  { value: 'container', label: 'A container' },
]

type Draft = { name: string; description: string; scope: PolicyScope; priority: number; enabled: boolean; profile: string; policy?: Policy; customized: boolean; version?: number }

function draftFrom(record?: PolicyRecord): Draft {
  if (!record) return { name: '', description: '', scope: { level: 'namespace', namespace: '' }, priority: 0, enabled: true, profile: 'balanced', customized: false }
  return { name: record.name, description: record.description ?? '', scope: record.scope, priority: record.priority, enabled: record.enabled, profile: 'balanced', policy: record.policy, customized: true, version: record.version }
}

function scopeComplete(scope: PolicyScope): boolean {
  switch (scope.level) {
    case 'global': return true
    case 'namespace': return Boolean(scope.namespace)
    case 'workload': return Boolean(scope.namespace && scope.kind && scope.workload)
    case 'container': return Boolean(scope.namespace && scope.kind && scope.workload && scope.container)
  }
}

function PolicyEditor({ record, profiles, onClose }: { record?: PolicyRecord; profiles: PolicyProfile[]; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [draft, setDraft] = useState<Draft>(() => draftFrom(record))
  const policy = draft.policy ?? profiles.find((item) => item.name === draft.profile)?.policy
  const update = (patch: Partial<Draft>) => setDraft((current) => ({ ...current, ...patch }))
  const updateScope = (patch: Partial<PolicyScope>) => setDraft((current) => ({ ...current, scope: { ...current.scope, ...patch } }))
  const save = useMutation({
    mutationFn: () => {
      const body = { name: draft.name.trim(), description: draft.description, scope: draft.scope, priority: draft.priority, enabled: draft.enabled, policy }
      return record
        ? api.put<PolicyRecord>(`/api/v1/policies/${encodeURIComponent(record.id)}`, { ...body, version: record.version })
        : api.post<PolicyRecord>('/api/v1/policies', body)
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['policies-admin'] })
      onClose()
    },
  })
  const level = draft.scope.level
  const valid = draft.name.trim() !== '' && scopeComplete(draft.scope) && Boolean(policy)
  return (
    <Dialog open onClose={save.isPending ? undefined : onClose} fullWidth maxWidth="md">
      <DialogTitle>{record ? `Edit ${record.name}` : 'New policy'}</DialogTitle>
      <DialogContent>
        <Stack sx={{ gap: 2, mt: 1 }}>
          {save.error && <RequestAlert error={save.error} resource="the policy" />}
          <Stack direction={{ xs: 'column', sm: 'row' }} sx={{ gap: 1.5 }}>
            <TextField label="Name" size="small" fullWidth required value={draft.name} onChange={(event) => update({ name: event.target.value })} />
            <TextField
              label="Priority" size="small" type="number" sx={{ minWidth: 140 }} value={draft.priority}
              onChange={(event) => update({ priority: Number(event.target.value) })}
              helperText="Higher wins at the same level"
            />
          </Stack>
          <TextField label="Description" size="small" fullWidth value={draft.description} onChange={(event) => update({ description: event.target.value })} />
          <Stack direction={{ xs: 'column', sm: 'row' }} sx={{ gap: 1.5 }}>
            <TextField select label="Applies to" size="small" fullWidth value={level} onChange={(event) => setDraft((current) => ({ ...current, scope: { level: event.target.value as PolicyLevel, cluster: current.scope.cluster } }))}>
              {levels.map((item) => <MenuItem key={item.value} value={item.value}>{item.label}</MenuItem>)}
            </TextField>
            <TextField label="Cluster" size="small" fullWidth value={draft.scope.cluster ?? ''} onChange={(event) => updateScope({ cluster: event.target.value || undefined })} helperText="Empty applies to every cluster" />
          </Stack>
          {level !== 'global' && (
            <Stack direction={{ xs: 'column', sm: 'row' }} sx={{ gap: 1.5 }}>
              <NamespaceSelect fullWidth required value={draft.scope.namespace ?? ''} onChange={(namespace) => updateScope({ namespace })} />
              {(level === 'workload' || level === 'container') && (
                <>
                  <TextField select label="Kind" size="small" fullWidth required value={draft.scope.kind ?? ''} onChange={(event) => updateScope({ kind: event.target.value })}>
                    {kinds.map((kind) => <MenuItem key={kind} value={kind}>{kind}</MenuItem>)}
                  </TextField>
                  <TextField label="Workload" size="small" fullWidth required value={draft.scope.workload ?? ''} onChange={(event) => updateScope({ workload: event.target.value })} />
                </>
              )}
              {level === 'container' && <TextField label="Container" size="small" fullWidth required value={draft.scope.container ?? ''} onChange={(event) => updateScope({ container: event.target.value })} />}
            </Stack>
          )}
          <FormControlLabel control={<Switch checked={draft.enabled} onChange={(event) => update({ enabled: event.target.checked })} />} label="Enabled" />
          {policy && (
            <PolicyTuner
              profiles={profiles} profile={draft.profile} policy={policy} customized={draft.customized}
              onChange={(profile, next, customized) => update({ profile, policy: customized ? next : undefined, customized })}
            />
          )}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={save.isPending}>Cancel</Button>
        <Button variant="contained" onClick={() => save.mutate()} disabled={!valid || save.isPending}>{record ? 'Save' : 'Create'}</Button>
      </DialogActions>
    </Dialog>
  )
}

function EffectivePolicy() {
  const [subject, setSubject] = useState<Subject>()
  const resolve = useMutation({ mutationFn: () => api.post<PolicyResolution>('/api/v1/policies:resolve', subject!) })
  const result = resolve.data
  return (
    <Card>
      <CardContent>
        <Typography variant="h6" component="h2">Effective policy</Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1.5 }}>
          Which policy a container gets: container, then workload, namespace, and global scope; a cluster-specific scope wins at the same level, then higher priority.
        </Typography>
        <WorkloadPicker value={subject} onChange={setSubject} />
        <Button variant="outlined" onClick={() => resolve.mutate()} disabled={!subject || resolve.isPending} sx={{ mt: 1.5 }}>Resolve</Button>
        {resolve.error && <Box sx={{ mt: 1.5 }}><RequestAlert error={resolve.error} resource="the effective policy" /></Box>}
        {result && (
          <Box sx={{ mt: 1.5 }} aria-label="Resolution">
            <Typography variant="body2">
              Applies: <strong>{result.source === 'built_in' ? `built-in ${result.name} profile` : result.matchedPolicy?.name}</strong>
              {result.matchedPolicy && ` · ${scopeLabel(result.matchedPolicy.scope)} · priority ${result.matchedPolicy.priority}`}
            </Typography>
            {result.ambiguous && <Alert severity="warning" variant="outlined" sx={{ mt: 1 }}>{result.warning}</Alert>}
            {(result.candidates?.length ?? 0) > 1 && (
              <Typography variant="caption" color="text.secondary" component="p" sx={{ mt: 0.5, overflowWrap: 'anywhere' }}>
                Also applicable, in order: {result.candidates!.slice(1).map((candidate) => candidate.name).join(', ')}
              </Typography>
            )}
          </Box>
        )}
      </CardContent>
    </Card>
  )
}

/** Persistent recommendation policies: scope, precedence, and tuning. */
export function Policies({ title, description }: { title: string; description: string }) {
  const queryClient = useQueryClient()
  const [editing, setEditing] = useState<PolicyRecord | 'new'>()
  const [deleting, setDeleting] = useState<PolicyRecord>()
  const query = useQuery({
    queryKey: ['policies-admin'],
    queryFn: ({ signal }) => api.get<{ items: PolicyListItem[] }>('/api/v1/policies?page=1&pageSize=100', { signal }),
  })
  const remove = useMutation({
    mutationFn: (record: PolicyRecord) => api.delete<unknown>(`/api/v1/policies/${encodeURIComponent(record.id)}`, { version: record.version }),
    onSuccess: async () => {
      setDeleting(undefined)
      await queryClient.invalidateQueries({ queryKey: ['policies-admin'] })
    },
  })
  const items = query.data?.items ?? []
  const profiles = items.filter((item): item is Extract<PolicyListItem, { builtIn: true }> => item.builtIn)
  const policies = items.filter((item): item is PolicyRecord & { builtIn: false } => !item.builtIn)
  return (
    <Box sx={{ p: { xs: 2, md: 4 }, maxWidth: 1500, mx: 'auto' }}>
      <PageHeader eyebrow="CONFIGURATION" title={title} description={description}>
        <Button sx={{ mt: 2 }} variant="contained" startIcon={<AddOutlined />} onClick={() => setEditing('new')} disabled={profiles.length === 0}>New policy</Button>
      </PageHeader>
      {query.isFetching && <LinearProgress sx={{ mb: 2 }} aria-label="Loading policies" />}
      {query.error && <Box sx={{ mb: 2 }}><RequestAlert error={query.error} resource="policies" onRetry={() => void query.refetch()} /></Box>}
      <Stack sx={{ gap: 3 }}>
        <Card>
          <CardContent>
            <Typography variant="h6" component="h2" sx={{ mb: 1 }}>Policies</Typography>
            {!query.isLoading && policies.length === 0 && (
              <Typography color="text.secondary">No persisted policies. Every container uses the built-in balanced profile until a policy applies to it.</Typography>
            )}
            {policies.length > 0 && (
              <TableContainer>
                <Table size="small" aria-label="Policies">
                  <TableHead>
                    <TableRow>
                      <TableCell>Name</TableCell>
                      <TableCell>Applies to</TableCell>
                      <TableCell align="right">Priority</TableCell>
                      <TableCell>Status</TableCell>
                      <TableCell>Updated</TableCell>
                      <TableCell align="right">Actions</TableCell>
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {policies.map((record) => (
                      <TableRow key={record.id}>
                        <TableCell sx={{ minWidth: 160 }}>
                          <Typography variant="body2" sx={{ fontWeight: 600 }}>{record.name}</Typography>
                          {record.description && <Typography variant="caption" color="text.secondary">{record.description}</Typography>}
                        </TableCell>
                        <TableCell sx={{ minWidth: 180, overflowWrap: 'anywhere' }}>
                          <Typography variant="body2">{scopeLabel(record.scope)}</Typography>
                          <Typography variant="caption" color="text.secondary">{record.scope.level} scope</Typography>
                        </TableCell>
                        <TableCell align="right">{record.priority}</TableCell>
                        <TableCell><Chip size="small" variant="outlined" color={record.enabled ? 'success' : 'default'} label={record.enabled ? 'Enabled' : 'Disabled'} /></TableCell>
                        <TableCell sx={{ minWidth: 150 }}>
                          <Typography variant="body2">{formatDate(record.updatedAt)}</Typography>
                          <Typography variant="caption" color="text.secondary">v{record.version} · {record.updatedBy}</Typography>
                        </TableCell>
                        <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
                          <Button size="small" onClick={() => setEditing(record)}>Edit</Button>
                          <Button size="small" color="error" onClick={() => setDeleting(record)}>Delete</Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </TableContainer>
            )}
          </CardContent>
        </Card>
        <EffectivePolicy />
        <Card>
          <CardContent>
            <Typography variant="h6" component="h2" sx={{ mb: 1 }}>Built-in profiles</Typography>
            <Stack sx={{ gap: 1 }}>
              {profiles.map((profile) => (
                <Typography key={profile.name} variant="body2"><strong>{profile.name}</strong>{profile.default ? ' (default)' : ''}: {profile.description}</Typography>
              ))}
            </Stack>
          </CardContent>
        </Card>
      </Stack>
      {editing && <PolicyEditor record={editing === 'new' ? undefined : editing} profiles={profiles} onClose={() => setEditing(undefined)} />}
      <Dialog open={Boolean(deleting)} onClose={() => setDeleting(undefined)}>
        <DialogTitle>Delete {deleting?.name}?</DialogTitle>
        <DialogContent>
          {remove.error && <Box sx={{ mb: 1 }}><RequestAlert error={remove.error} resource="the policy" /></Box>}
          <DialogContentText>Containers it applies to fall back to the next applicable policy or the built-in balanced profile. Existing recommendations keep the policy they were calculated with.</DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setDeleting(undefined)}>Cancel</Button>
          <Button color="error" variant="contained" onClick={() => deleting && remove.mutate(deleting)} disabled={remove.isPending}>Delete</Button>
        </DialogActions>
      </Dialog>
    </Box>
  )
}
