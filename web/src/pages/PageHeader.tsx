import type { ReactNode } from 'react'
import { Box, Typography } from '@mui/material'

export function PageHeader({ eyebrow, title, description, children }: { eyebrow: string; title: string; description: string; children?: ReactNode }) {
  return (
    <Box sx={{ mb: 3 }}>
      <Typography variant="overline" color="primary.main">{eyebrow}</Typography>
      <Typography variant="h3" component="h1" sx={{ fontSize: { xs: '2.125rem', sm: '3rem' }, overflowWrap: 'anywhere' }}>{title}</Typography>
      <Typography color="text.secondary" sx={{ mt: 1, maxWidth: 760 }}>{description}</Typography>
      {children}
    </Box>
  )
}
