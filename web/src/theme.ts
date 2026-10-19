import { createTheme } from '@mui/material/styles'

export const theme = createTheme({
  palette: {
    mode: 'dark',
    primary: { main: '#66d9c4', light: '#9df3e1', dark: '#20a78f' },
    secondary: { main: '#ffb45e' },
    background: { default: '#07111c', paper: '#0d1b28' },
    success: { main: '#74d99f' },
    warning: { main: '#ffbf69' },
    error: { main: '#ff758f' },
    text: { primary: '#eef8f6', secondary: '#a9bec8' },
  },
  typography: {
    fontFamily: 'Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
    h1: { fontWeight: 750, letterSpacing: '-0.04em' },
    h2: { fontWeight: 720, letterSpacing: '-0.035em' },
    h3: { fontWeight: 700, letterSpacing: '-0.025em' },
    button: { textTransform: 'none', fontWeight: 700 },
  },
  shape: { borderRadius: 12 },
  components: {
    MuiCard: {
      styleOverrides: {
        root: {
          backgroundImage: 'linear-gradient(145deg, rgba(18,40,55,.94), rgba(10,25,38,.98))',
          border: '1px solid rgba(148, 188, 196, .14)',
        },
      },
    },
    MuiButton: { defaultProps: { disableElevation: true } },
    MuiTableCell: { styleOverrides: { root: { borderColor: 'rgba(148,188,196,.12)' } } },
  },
})
