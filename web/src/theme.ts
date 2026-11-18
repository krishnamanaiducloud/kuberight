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
    MuiCssBaseline: {
      styleOverrides: {
        body: { scrollbarColor: '#496675 #07111c' },
        '::selection': { backgroundColor: 'rgba(102,217,196,.28)' },
        '@media (prefers-reduced-motion: reduce)': {
          '*, *::before, *::after': { scrollBehavior: 'auto !important', transitionDuration: '0.01ms !important', animationDuration: '0.01ms !important', animationIterationCount: '1 !important' },
        },
      },
    },
    MuiButtonBase: {
      styleOverrides: {
        root: { '&.Mui-focusVisible': { outline: '3px solid rgba(157,243,225,.72)', outlineOffset: 2 } },
      },
    },
    MuiCard: {
      styleOverrides: {
        root: {
          backgroundImage: 'linear-gradient(145deg, rgba(18,40,55,.94), rgba(10,25,38,.98))',
          border: '1px solid rgba(148, 188, 196, .14)',
        },
      },
    },
    MuiButton: { defaultProps: { disableElevation: true } },
    MuiTableCell: { styleOverrides: { root: { borderColor: 'rgba(148,188,196,.12)' }, head: { color: '#cde0e5' } } },
    // Tables scroll horizontally on narrow screens; keyboard users must be
    // able to focus the region to scroll it (WCAG 2.1.1).
    MuiTableContainer: {
      defaultProps: { tabIndex: 0 },
      styleOverrides: { root: { '&:focus-visible': { outline: '3px solid rgba(157,243,225,.72)', outlineOffset: 2 } } },
    },
    // Tooltips add a description; they never replace a control's visible
    // name (WCAG 2.5.3). Icon-only controls carry their own aria-label.
    MuiTooltip: { defaultProps: { describeChild: true } },
    // A spinner is announced as a progress bar and needs a name (WCAG 4.1.2).
    MuiCircularProgress: { defaultProps: { 'aria-label': 'Loading' } },
  },
})
