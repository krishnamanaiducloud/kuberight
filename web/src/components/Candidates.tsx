import { Box, Chip, Table, TableBody, TableCell, TableContainer, TableHead, TableRow, Typography } from '@mui/material'
import type { Candidate } from '../api'
import { formatBytes, formatMillicores } from '../format'

const profileLabel: Record<Candidate['profile'], string> = { conservative: 'Conservative', balanced: 'Balanced', aggressive: 'Aggressive' }
const percent = (value?: number) => (value === undefined ? '—' : `${(value * 100).toFixed(1)}%`)
const change = (value: number) => (value === 0 ? 'no change' : `${value > 0 ? '+' : ''}${value.toFixed(0)}%`)

/**
 * The configuration each stock profile would produce from the same
 * observations. The held-out columns size each candidate from the first 70%
 * of samples and count how often the remaining 30% exceeded it.
 */
export function CandidatesTable({ candidates }: { candidates: Candidate[] }) {
  if (candidates.length === 0) return null
  return (
    <Box>
      <Typography variant="subtitle2">Alternatives</Typography>
      <Typography variant="caption" color="text.secondary" component="p" sx={{ mb: 1 }}>
        What each profile would set from the same data, within your policy's bounds and change guards, after the same safety checks. Held-out columns show how often later usage exceeded a candidate sized from earlier usage. For comparison only; only the recommendation can be approved.
      </Typography>
      <TableContainer tabIndex={0} role="region" aria-label="Alternative configurations">
        <Table size="small" aria-label="Alternative configurations by profile">
          <TableHead>
            <TableRow>
              <TableCell>Profile</TableCell>
              <TableCell align="right">CPU request</TableCell>
              <TableCell align="right">Memory request / limit</TableCell>
              <TableCell align="right">CPU over request (held out)</TableCell>
              <TableCell align="right">Memory over limit (held out)</TableCell>
              <TableCell align="right">Risk</TableCell>
              <TableCell>Verdict</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {candidates.map((candidate) => (
              <TableRow key={candidate.profile} selected={candidate.selected}>
                <TableCell>
                  {profileLabel[candidate.profile]}
                  {candidate.selected && <Chip size="small" variant="outlined" color="primary" label="Recommended" sx={{ ml: 1 }} />}
                </TableCell>
                <TableCell align="right">{formatMillicores(candidate.cpuRequestMilli)} ({change(candidate.estimatedCpuChangePercent)})</TableCell>
                <TableCell align="right">
                  {formatBytes(candidate.memoryRequestBytes)} ({change(candidate.estimatedMemoryChangePercent)}) / {candidate.memoryLimitBytes > 0 ? formatBytes(candidate.memoryLimitBytes) : 'none'}
                </TableCell>
                <TableCell align="right">{candidate.outOfSample.performed ? percent(candidate.outOfSample.cpuOverRequest) : '—'}</TableCell>
                <TableCell align="right">{candidate.outOfSample.performed ? percent(candidate.outOfSample.memoryOverLimit) : '—'}</TableCell>
                <TableCell align="right">{candidate.riskScore === undefined ? '—' : Math.round(candidate.riskScore)}</TableCell>
                <TableCell>
                  {candidate.status ?? '—'}
                  {(candidate.statusReasons?.length ?? 0) > 0 && (
                    <Typography variant="caption" color="text.secondary" component="p">{candidate.statusReasons?.join('; ')}</Typography>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </TableContainer>
    </Box>
  )
}
