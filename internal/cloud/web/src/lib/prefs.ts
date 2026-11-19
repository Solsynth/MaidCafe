/** Persisted interface preferences: color theme and language. */

export type Theme = 'dark' | 'light'
export type Locale = 'en' | 'zh-CN'

export const THEME_KEY = 'maidkit.theme'
export const LOCALE_KEY = 'maidkit.locale'

export const themeNames: Record<Theme, string> = {
  dark: 'maidkit',
  light: 'maidkit-light',
}

export const themeColors: Record<Theme, string> = {
  dark: '#171618',
  light: '#faf7f9',
}

function storage(): Storage | null {
  return typeof localStorage === 'undefined' ? null : localStorage
}

export function readTheme(): Theme {
  const stored = storage()?.getItem(THEME_KEY)
  if (stored === 'light' || stored === 'dark') return stored
  if (typeof window === 'undefined' || !window.matchMedia) return 'dark'
  return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
}

export function applyTheme(theme: Theme) {
  if (typeof document === 'undefined') return
  document.documentElement.dataset.theme = themeNames[theme]
  const meta = document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')
  if (meta) {
    meta.removeAttribute('media')
    meta.content = themeColors[theme]
  }
}

export function readLocale(): Locale {
  const stored = storage()?.getItem(LOCALE_KEY)
  if (stored === 'en' || stored === 'zh-CN') return stored
  if (typeof navigator === 'undefined') return 'en'
  const languages = navigator.languages?.length ? navigator.languages : [navigator.language]
  return languages.some((language) => language.toLowerCase().startsWith('zh')) ? 'zh-CN' : 'en'
}

export function applyLocale(locale: Locale) {
  if (typeof document === 'undefined') return
  document.documentElement.lang = locale
}
