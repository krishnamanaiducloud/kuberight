import { Component, type ErrorInfo, type ReactNode } from 'react'
import { Alert, AlertTitle, Box, Button, Typography } from '@mui/material'

type Props = { children: ReactNode; resetKey: string }
type State = { error: Error | null; resetKey: string }

// Contains a view that fails to render, so the navigation stays usable and
// the next route renders normally.
export class RouteErrorBoundary extends Component<Props, State> {
  state: State = { error: null, resetKey: this.props.resetKey }

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error }
  }

  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    return props.resetKey !== state.resetKey ? { error: null, resetKey: props.resetKey } : null
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('KubeRight view failed to render', error, info.componentStack)
  }

  render() {
    if (!this.state.error) return this.props.children
    return (
      <Box sx={{ maxWidth: 760 }}>
        <Typography component="h1" variant="h4" sx={{ mb: 2 }}>This view could not be displayed</Typography>
        <Alert severity="error" action={<Button color="inherit" size="small" onClick={() => this.setState({ error: null })}>Try again</Button>}>
          <AlertTitle>Unexpected response or rendering error</AlertTitle>
          {this.state.error.message || 'The view failed to render.'} Other views remain available from the navigation.
        </Alert>
      </Box>
    )
  }
}
