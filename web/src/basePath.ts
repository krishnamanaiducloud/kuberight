/**
 * The URL prefix KubeRight is served under, such as "/kuberight", or "" at
 * the root. The server writes it into <base href> (KUBERIGHT_BASE_PATH).
 */
export function currentBasePath(): string {
  if (typeof document === 'undefined') return ''
  return new URL(document.baseURI).pathname.replace(/\/+$/, '')
}

export const basePath = currentBasePath()

/** An absolute application path, such as "/api/v1/version", under the prefix. */
export function appUrl(path: string): string {
  return path.startsWith('/') ? `${basePath}${path}` : path
}
