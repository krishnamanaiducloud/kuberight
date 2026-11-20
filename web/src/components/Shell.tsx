import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link as RouterLink, useLocation } from 'react-router-dom'
import {
  AppBar, Box, Button, Chip, Divider, Drawer, IconButton, List, ListItemButton,
  ListItemIcon, ListItemText, Toolbar, Typography, useMediaQuery,
} from '@mui/material'
import MenuIcon from '@mui/icons-material/Menu'
import ShieldOutlined from '@mui/icons-material/ShieldOutlined'
import { useTheme } from '@mui/material/styles'
import { navigation } from '../navigation'
import { api } from '../api'

const drawerWidth = 272

export function Shell({ children }: { children: ReactNode }) {
  const location = useLocation()
  const theme = useTheme()
  const desktop = useMediaQuery(theme.breakpoints.up('lg'))
  const [drawerOpen, setDrawerOpen] = useState(false)
	// KubeRight has no sign-in, so the mode is unknown only while the API is
	// starting or unreachable; keep asking until it answers.
	const modeQuery = useQuery({
		queryKey: ['version'],
		queryFn: ({ signal }) => api.get<{ mode?: string }>('/api/v1/version', { signal }),
		retry: 3,
		refetchInterval: (query) => (query.state.data?.mode ? false : 15_000),
		staleTime: 60_000,
	})
	const operationalMode = modeQuery.data?.mode
	const modeLabel = operationalMode
		? `${operationalMode[0].toUpperCase()}${operationalMode.slice(1)}`
		: modeQuery.isError ? 'Unreachable' : 'Checking'
	const modeDescription = operationalMode === 'observe'
		? 'Recommendation generation and workload changes are disabled.'
		: operationalMode === 'recommend'
			? 'Read-only: KubeRight never changes workloads.'
			: modeQuery.isError
				? 'The KubeRight API is not answering yet. Retrying.'
				: 'Asking the KubeRight API for its mode.'

  const drawer = (
    <Box sx={{ height: '100%', display: 'flex', flexDirection: 'column' }}>
      <Toolbar sx={{ minHeight: 76, gap: 1.5 }}>
        <Box aria-hidden sx={{ width: 34, height: 34, borderRadius: 2, bgcolor: 'primary.main', color: '#051718', display: 'grid', placeItems: 'center', fontWeight: 900 }}>K</Box>
        <Box>
          <Typography variant="h6" sx={{ fontWeight: 800, lineHeight: 1.05 }}>KubeRight</Typography>
          <Typography variant="caption" color="text.secondary">Resource intelligence</Typography>
        </Box>
      </Toolbar>
      <Divider />
      <List component="nav" aria-label="Primary navigation" sx={{ px: 1.25, py: 1.5, overflowY: 'auto' }}>
        {navigation.map((item) => {
          const active = location.pathname === item.path || location.pathname.startsWith(`${item.path}/`)
          return (
            <ListItemButton
              key={item.path}
              component={RouterLink}
              to={item.path}
              selected={active}
              onClick={() => setDrawerOpen(false)}
              sx={{ borderRadius: 2, mb: .25, minHeight: 42, '&.Mui-selected': { bgcolor: 'rgba(102,217,196,.14)', color: 'primary.light' } }}
            >
              <ListItemIcon sx={{ minWidth: 38, color: active ? 'primary.main' : 'text.secondary' }}>{item.icon}</ListItemIcon>
              <ListItemText primary={item.label} slotProps={{ primary: { sx: { fontSize: 13.5, fontWeight: active ? 750 : 550 } } }} />
            </ListItemButton>
          )
        })}
      </List>
      <Box sx={{ mt: 'auto', p: 2 }}>
        <Box sx={{ p: 1.5, border: '1px solid', borderColor: 'divider', borderRadius: 2, bgcolor: 'rgba(255,255,255,.02)' }}>
          <Typography variant="caption" color="text.secondary">Operational mode</Typography>
          <Typography sx={{ fontWeight: 800, color: 'secondary.main' }}>{modeLabel}</Typography>
          <Typography variant="caption" color="text.secondary">{modeDescription}</Typography>
        </Box>
      </Box>
    </Box>
  )

  return (
    <Box sx={{ display: 'flex', minHeight: '100vh' }}>
      <Button
        component="a"
        href="#main-content"
        variant="contained"
        sx={{ position: 'fixed', zIndex: theme.zIndex.modal + 1, top: 8, left: 8, transform: 'translateY(-150%)', '&:focus': { transform: 'translateY(0)' } }}
      >
        Skip to main content
      </Button>
      <AppBar position="fixed" elevation={0} sx={{ ml: { lg: `${drawerWidth}px` }, width: { lg: `calc(100% - ${drawerWidth}px)` }, bgcolor: 'rgba(7,17,28,.88)', backdropFilter: 'blur(16px)', borderBottom: '1px solid rgba(148,188,196,.12)' }}>
        <Toolbar sx={{ minHeight: 76 }}>
          {!desktop && <IconButton aria-label="Open navigation" onClick={() => setDrawerOpen(true)} sx={{ mr: 1 }}><MenuIcon /></IconButton>}
          <Box sx={{ flexGrow: 1 }}>
            <Typography variant="caption" color="text.secondary">CONTROL PLANE</Typography>
            <Typography sx={{ fontWeight: 750 }}>Verified signals only</Typography>
          </Box>
          <Chip icon={<ShieldOutlined />} label={`${modeLabel} mode`} color="warning" variant="outlined" sx={{ display: { xs: 'none', sm: 'flex' } }} />
        </Toolbar>
      </AppBar>
      <Box component="aside" sx={{ width: { lg: drawerWidth }, flexShrink: { lg: 0 } }}>
        <Drawer variant={desktop ? 'permanent' : 'temporary'} open={desktop || drawerOpen} onClose={() => setDrawerOpen(false)} ModalProps={{ keepMounted: true }} sx={{ '& .MuiDrawer-paper': { width: drawerWidth, boxSizing: 'border-box', bgcolor: '#091621', borderRightColor: 'rgba(148,188,196,.12)' } }}>{drawer}</Drawer>
      </Box>
      <Box id="main-content" tabIndex={-1} component="main" sx={{ flexGrow: 1, width: { lg: `calc(100% - ${drawerWidth}px)` }, pt: '76px', minWidth: 0, outline: 'none' }}>
        {children}
      </Box>
    </Box>
  )
}
