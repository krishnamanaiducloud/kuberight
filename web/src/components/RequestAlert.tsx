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

  // KubeRight has no sign-in, so a 403 means its own ServiceAccount cannot
  // read this scope in Kubernetes.
  if (apiError?.status === 403) {
    return (
      <Alert severity="warning" action={retry}>
        <AlertTitle>KubeRight cannot read this scope</AlertTitle>
        {namespaceScoped
          ? `KubeRight's ServiceAccount has no read access to ${resource} in this namespace. Choose another namespace, or install with rbac.scope=cluster.`
          : `KubeRight's ServiceAccount has no read access to ${resource}.`}
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
