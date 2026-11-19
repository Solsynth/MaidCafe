import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

import { i18n } from '@/lib/i18n'
import {
  LOCALE_KEY,
  THEME_KEY,
  applyLocale,
  applyTheme,
  readLocale,
  readTheme,
  type Locale,
  type Theme,
} from '@/lib/prefs'

export const useUiStore = defineStore('ui', () => {
  const theme = ref<Theme>(readTheme())
  const locale = ref<Locale>(readLocale())

  const isDark = computed(() => theme.value === 'dark')

  function setTheme(next: Theme) {
    theme.value = next
    localStorage.setItem(THEME_KEY, next)
    applyTheme(next)
  }

  function toggleTheme() {
    setTheme(theme.value === 'dark' ? 'light' : 'dark')
  }

  function setLocale(next: Locale) {
    locale.value = next
    i18n.global.locale.value = next
    localStorage.setItem(LOCALE_KEY, next)
    applyLocale(next)
  }

  return { theme, locale, isDark, setTheme, toggleTheme, setLocale }
})
