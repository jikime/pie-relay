export class WebChatAPIError extends Error {
  constructor(message: string, public readonly status: number) {
    super(message)
    this.name = 'WebChatAPIError'
  }
}

export async function webChatAPI<T>(
  path: string,
  { method = 'GET', body, csrfToken = '', signal }: {
    method?: string
    body?: unknown
    csrfToken?: string
    signal?: AbortSignal
  } = {},
): Promise<T> {
  const headers = new Headers({ Accept: 'application/json' })
  if (body !== undefined) headers.set('Content-Type', 'application/json')
  if (csrfToken && method !== 'GET' && method !== 'HEAD') headers.set('X-CSRF-Token', csrfToken)
  const response = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: 'same-origin',
    cache: 'no-store',
    signal,
  })
  const text = await response.text()
  let value: unknown = null
  try { value = text ? JSON.parse(text) : null } catch { /* handled by the status check */ }
  if (!response.ok) {
    const message = typeof value === 'object' && value && 'error' in value && typeof value.error === 'string'
      ? value.error
      : `HTTP ${response.status}`
    throw new WebChatAPIError(message, response.status)
  }
  return value as T
}
