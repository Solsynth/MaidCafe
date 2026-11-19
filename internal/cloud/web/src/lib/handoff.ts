/**
 * The receiving half of the identity hand-off.
 *
 * The identity console cannot set a cookie on this domain, so it sends the
 * browser back here with a single-use `code`. The code is stripped from the
 * address bar as soon as it has been read: a code left in the history is a
 * credential left on screen.
 */

const HANDOFF_PARAMS = ['code', 'state', 'error', 'error_description'] as const

export function handoffCode(search: string): string | null {
  const code = new URLSearchParams(search).get('code')
  return code && code.trim() ? code : null
}

export function stripHandoffParams(href: string): string {
  const url = new URL(href)
  for (const name of HANDOFF_PARAMS) url.searchParams.delete(name)
  return url.toString()
}

export function currentPageUrl(): string {
  return stripHandoffParams(window.location.href)
}

export function clearHandoffParams(): void {
  const stripped = stripHandoffParams(window.location.href)
  if (stripped === window.location.href) return
  window.history.replaceState(window.history.state, '', stripped)
}

/** A sign-in failure carried back by the issuer instead of a code. */
export function handoffError(search: string): string | null {
  const params = new URLSearchParams(search)
  const error = params.get('error')
  if (!error) return null
  return params.get('error_description') || error
}
