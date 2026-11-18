import { Alert, AlertTitle, Button } from '@mui/material'
import { ApiError } from '../api'

type RequestAlertProps = {
  error: unknown
  resource: string
  namespaceScoped?: boolean
  onRetry?: () => void
}

/** Consistent, actionable API failures without exposing implementation detail. */
export function RequestAlert({ error, resource, namespaceScoped, onRetry }: RequestAlertProps) {
  const apiError = error instanceof ApiError ? error : undefined
  const request = apiError?.requestId ? ` Request ID: ${apiError.requestId}.` : ''
  const retry = onRetry ? <Button color="inherit" size="small" onClick={onRetry}>Retry</Button> : undefined

  if (apiError?.status === 401) {
    return (
      <Alert severity="warning" action={retry}>
        <AlertTitle>Connect to view {resource}</AlertTitle>
        Your session has no valid bearer token. Use <strong>Connect</strong> in the header, then try again.{request}
      </Alert>
    )
  }

  if (apiError?.status === 403) {
    return (
      <Alert severity="warning" action={retry}>
        <AlertTitle>This identity cannot view this scope</AlertTitle>
        {namespaceScoped
          ? `Choose a namespace this identity can access, or ask an administrator for list permission on ${resource} in this namespace.`
          : `Ask an administrator for permission to view ${resource}.`}
        {request}
      </Alert>
    )
  }

  return (
    <Alert severity="error" action={retry}>
      <AlertTitle>{resource} could not be verified</AlertTitle>
      {error instanceof Error ? error.message : 'The control plane returned an unknown error.'}{request}
    </Alert>
  )
}
