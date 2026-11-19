import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

import type { Account } from '@/lib/api/types'
import { CloudError, onSessionRefresh, onUnauthorized } from '@/lib/api/client'
import { sessionApi } from '@/lib/api'
import { clearHandoffParams, handoffCode, handoffError } from '@/lib/handoff'
import { translate } from '@/lib/i18n'
import { useNoticeStore } from '@/stores/notice'

type SessionState = 'loading' | 'ready' | 'signed-out' | 'offline'

export const useSessionStore = defineStore('session', () => {
  const state = ref<SessionState>('loading')
  const account = ref<Account | null>(null)
  const error = ref('')

  let booting: Promise<void> | null = null
  let booted = false

  const signedIn = computed(() => state.value === 'ready' && account.value !== null)
  const displayName = computed(
    () => account.value?.nick || account.value?.name || account.value?.id || '',
  )

  // A 401 on an ordinary request is handled by the API client, which renews
  // once and repeats. Only a refused refresh lands here.
  onUnauthorized(() => {
    clear()
  })

  onSessionRefresh(async () => {
    try {
      const session = await sessionApi.refresh()
      account.value = session.account
      state.value = 'ready'
      error.value = ''
      return true
    } catch {
      clear()
      return false
    }
  })

  async function load() {
    try {
      const session = await sessionApi.me()
      account.value = session.account
      state.value = 'ready'
      error.value = ''
    } catch (caught) {
      account.value = null
      if (caught instanceof CloudError && caught.isUnauthenticated) {
        state.value = 'signed-out'
        error.value = ''
        return
      }
      state.value = 'offline'
      error.value = caught instanceof Error ? caught.message : translate('app.offlineBody')
    }
  }

  function clear() {
    account.value = null
    state.value = 'signed-out'
    error.value = ''
  }

  async function completeHandoff(code: string): Promise<boolean> {
    // Strip the code before the network call: a failure must not leave a
    // single-use credential in the address bar for a reload to replay.
    clearHandoffParams()
    try {
      const session = await sessionApi.redeem(code)
      account.value = session.account
      state.value = 'ready'
      error.value = ''
      return true
    } catch (caught) {
      clear()
      const message = caught instanceof Error ? caught.message : translate('app.signInFailed')
      useNoticeStore().fail(message)
      return false
    }
  }

  function boot(): Promise<void> {
    if (booted) return Promise.resolve()
    if (!booting) {
      booting = runBoot().finally(() => {
        booting = null
        booted = true
      })
    }
    return booting
  }

  async function runBoot() {
    const returnedError = handoffError(window.location.search)
    if (returnedError) {
      clearHandoffParams()
      clear()
      useNoticeStore().fail(returnedError)
      return
    }
    const code = handoffCode(window.location.search)
    if (code) {
      await completeHandoff(code)
      return
    }
    await load()
  }

  async function signOut() {
    try {
      await sessionApi.logout()
    } catch {
      // Cookies are HttpOnly; the console still shows the signed-out state,
      // and the next request decides whether the server agrees.
    }
    clear()
    useNoticeStore().ok(translate('app.signedOutNotice'))
  }

  async function retry() {
    state.value = 'loading'
    await load()
  }

  return {
    state,
    account,
    error,
    signedIn,
    displayName,
    boot,
    load,
    completeHandoff,
    signOut,
    retry,
    clear,
  }
})
