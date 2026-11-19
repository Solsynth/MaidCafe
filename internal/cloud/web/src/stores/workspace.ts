import { computed, ref, watch } from 'vue'
import { defineStore } from 'pinia'

import type { Quota, Workspace } from '@/lib/api/types'
import { CloudError } from '@/lib/api/client'
import { workspaceApi } from '@/lib/api'
import { useSessionStore } from '@/stores/session'

const SELECTED_KEY = 'maidkit.workspace'

export const useWorkspaceStore = defineStore('workspace', () => {
  const list = ref<Workspace[]>([])
  const selectedID = ref(localStorage.getItem(SELECTED_KEY) ?? '')
  const quota = ref<Quota | null>(null)
  const loading = ref(false)
  const loaded = ref(false)
  const error = ref('')

  const selected = computed(
    () => list.value.find((workspace) => workspace.id === selectedID.value) ?? null,
  )

  async function load(force = false) {
    const session = useSessionStore()
    if (!session.signedIn) return
    if (loaded.value && !force) return
    loading.value = true
    error.value = ''
    try {
      list.value = await workspaceApi.list()
      if (!list.value.some((workspace) => workspace.id === selectedID.value)) {
        selectedID.value = list.value[0]?.id ?? ''
      }
      loaded.value = true
      await loadQuota()
    } catch (caught) {
      list.value = []
      error.value =
        caught instanceof CloudError
          ? caught.message
          : 'Could not read the account workspaces.'
    } finally {
      loading.value = false
    }
  }

  async function loadQuota() {
    if (!selectedID.value) {
      quota.value = null
      return
    }
    try {
      quota.value = await workspaceApi.quota(selectedID.value)
    } catch {
      quota.value = null
    }
  }

  function select(id: string) {
    selectedID.value = id
  }

  watch(selectedID, (id) => {
    if (id) localStorage.setItem(SELECTED_KEY, id)
    else localStorage.removeItem(SELECTED_KEY)
    void loadQuota()
  })

  return { list, selectedID, selected, quota, loading, loaded, error, load, loadQuota, select }
})
