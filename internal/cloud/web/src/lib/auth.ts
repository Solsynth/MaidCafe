import { currentPageUrl } from '@/lib/handoff'
import { sessionApi } from '@/lib/api'
import type { AuthConfig } from '@/lib/api'

const DEFAULT_AUTH_PAGE = 'https://id.solian.app/sign-in'

let cachedConfig: AuthConfig | null = null

/** The issuer's sign-in configuration, read once per page load. */
export async function authConfig(): Promise<AuthConfig> {
  if (cachedConfig) return cachedConfig
  try {
    cachedConfig = await sessionApi.config()
  } catch {
    cachedConfig = { configured: false, sign_in_url: '' }
  }
  return cachedConfig
}

/**
 * The address of the shared sign-in page, returning to the page the person is
 * on. No token or secret is placed here — the issuer validates the host before
 * it issues a code, and this console only ever sees the code.
 */
export async function signInUrl(): Promise<string> {
  const config = await authConfig()
  const base = config.configured
    ? config.sign_in_url
    : import.meta.env.VITE_AUTH_URL || DEFAULT_AUTH_PAGE
  try {
    const url = new URL(base, window.location.origin)
    url.searchParams.set('redirectUri', currentPageUrl())
    return url.toString()
  } catch {
    return base
  }
}

/** Starts the redirect; a failed config read still leaves a usable page. */
export async function beginSignIn(): Promise<void> {
  window.location.href = await signInUrl()
}
