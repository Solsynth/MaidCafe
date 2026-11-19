import { translate } from '@/lib/i18n'

/** A typed failure from the MaidKit cloud API. */
export class CloudError extends Error {
  readonly status: number
  readonly detail?: string
  readonly code?: string

  constructor(status: number, message: string, detail?: string, code?: string) {
    super(message)
    this.name = 'CloudError'
    this.status = status
    this.detail = detail
    this.code = code
  }

  get isUnauthenticated(): boolean {
    return this.status === 401
  }

  get isForbidden(): boolean {
    return this.status === 403
  }

  get isNotFound(): boolean {
    return this.status === 404
  }
}

type Method = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'

export type QueryValue = string | number | boolean | null | undefined

export interface RequestOptions {
  method?: Method
  body?: unknown
  query?: Record<string, QueryValue>
  signal?: AbortSignal
  retryOnUnauthorized?: boolean
}

let unauthorizedHandler: (() => void) | null = null
let refreshHandler: (() => Promise<boolean>) | null = null
let refreshing: Promise<boolean> | null = null

export function onUnauthorized(handler: () => void) {
  unauthorizedHandler = handler
}

/** Installed by the session store: a 401 means renew, not signed out. */
export function onSessionRefresh(handler: () => Promise<boolean>) {
  refreshHandler = handler
}

function refreshSession(): Promise<boolean> {
  if (!refreshing) {
    const attempt = refreshHandler ? refreshHandler() : Promise.resolve(false)
    refreshing = attempt
      .catch(() => false)
      .finally(() => {
        refreshing = null
      })
  }
  return refreshing
}

export function buildUrl(path: string, query?: Record<string, QueryValue>): string {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query ?? {})) {
    if (value === undefined || value === null || value === '') continue
    params.set(key, String(value))
  }
  const suffix = params.size ? `?${params.toString()}` : ''
  return `/api${path}${suffix}`
}

async function readJson<T>(response: Response): Promise<T> {
  if (response.status === 204) return undefined as T
  const text = await response.text()
  if (!text) return undefined as T
  return JSON.parse(text) as T
}

async function parseError(response: Response): Promise<CloudError> {
  const fallback = new CloudError(
    response.status,
    response.statusText || translate('api.requestFailed'),
  )
  try {
    const payload = (await response.json()) as {
      error?: string
      error_description?: string
      message?: string
      code?: string
    }
    const message = payload.error_description || payload.message || payload.error || fallback.message
    return new CloudError(response.status, message, payload.error, payload.code)
  } catch {
    return fallback
  }
}

async function send(url: string, init: RequestInit): Promise<Response> {
  let response: Response
  try {
    response = await fetch(url, {
      credentials: 'same-origin',
      headers: { Accept: 'application/json', ...(init.headers ?? {}) },
      ...init,
    })
  } catch {
    throw new CloudError(0, translate('api.offline'))
  }
  if (response.ok) return response
  throw await parseError(response)
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = {}
  let body: BodyInit | undefined
  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(options.body)
  }
  const url = buildUrl(path, options.query)
  const init: RequestInit = {
    method: options.method ?? 'GET',
    headers,
    body,
    signal: options.signal,
  }

  try {
    const response = await send(url, init)
    return await readJson<T>(response)
  } catch (error) {
    const cloudError = error instanceof CloudError ? error : new CloudError(0, translate('api.offline'))
    const retry = options.retryOnUnauthorized !== false
    if (retry && cloudError.isUnauthenticated && (await refreshSession())) {
      const response = await send(url, init)
      return await readJson<T>(response)
    }
    if (cloudError.isUnauthenticated) unauthorizedHandler?.()
    throw cloudError
  }
}
