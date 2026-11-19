import { createI18n } from 'vue-i18n'

import { readLocale } from '@/lib/prefs'
import en from '@/locales/en/app'
import zhCN from '@/locales/zh-CN/app'

export const i18n = createI18n<false>({
  legacy: false,
  locale: readLocale(),
  fallbackLocale: 'en',
  messages: { en, 'zh-CN': zhCN },
})

export function translate(key: string, named?: Record<string, unknown>): string {
  return i18n.global.t(key, named ?? {})
}
