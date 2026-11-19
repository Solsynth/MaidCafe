<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Add16, ChevronRight16, Cloud16, Renew16 } from '@carbon/icons-vue'
import { RouterLink } from 'vue-router'

import type { Daemon, DaemonCredential } from '@/lib/api/types'
import { CloudError } from '@/lib/api/client'
import { daemonApi } from '@/lib/api'
import { relative, stamp } from '@/lib/format'
import { useWorkspaceStore } from '@/stores/workspace'
import { useNoticeStore } from '@/stores/notice'
import SessionGate from '@/components/SessionGate.vue'
import PageHeader from '@/components/PageHeader.vue'
import ConsolePanel from '@/components/ConsolePanel.vue'
import LoadingSkeleton from '@/components/LoadingSkeleton.vue'
import StatusPill from '@/components/StatusPill.vue'
import CopyButton from '@/components/CopyButton.vue'

const { t } = useI18n()
const workspace = useWorkspaceStore()
const notice = useNoticeStore()

const daemons = ref<Daemon[]>([])
const loading = ref(false)
const error = ref('')
const creating = ref(false)
const createOpen = ref(false)
const createName = ref('')
const created = ref<DaemonCredential | null>(null)

const quota = computed(() => workspace.quota?.quotas ?? {})
const daemonCount = computed(() => daemons.value.length)
const limit = computed(() => quota.value.max_daemons ?? 0)

function daemonTone(daemon: Daemon): 'success' | 'warning' | 'error' | 'neutral' {
  if (!daemon.enabled) return 'neutral'
  if (!daemon.last_seen_at) return 'warning'
  const age = Date.now() - new Date(daemon.last_seen_at).getTime()
  return age > 5 * 60 * 1000 ? 'error' : 'success'
}

function daemonState(daemon: Daemon): string {
  if (!daemon.enabled) return t('fleet.disabled')
  if (!daemon.last_seen_at) return t('fleet.neverSeen')
  const age = Date.now() - new Date(daemon.last_seen_at).getTime()
  return age > 5 * 60 * 1000 ? t('fleet.stale') : t('fleet.live')
}

async function load() {
  if (!workspace.selectedID) {
    daemons.value = []
    return
  }
  loading.value = true
  error.value = ''
  try {
    daemons.value = await daemonApi.list(workspace.selectedID)
  } catch (caught) {
    daemons.value = []
    error.value = caught instanceof CloudError ? caught.message : t('api.requestFailed')
  } finally {
    loading.value = false
  }
}

async function create() {
  const name = createName.value.trim()
  if (!name || !workspace.selectedID) return
  creating.value = true
  try {
    created.value = await daemonApi.create(workspace.selectedID, name)
    createOpen.value = false
    createName.value = ''
    notice.ok(t('fleet.secretTitle'))
    await load()
    await workspace.loadQuota()
  } catch (caught) {
    notice.fail(caught instanceof Error ? caught.message : t('api.requestFailed'))
  } finally {
    creating.value = false
  }
}

onMounted(async () => {
  await workspace.load()
  await load()
})

watch(
  () => workspace.selectedID,
  () => void load(),
)
</script>

<template>
  <SessionGate>
    <PageHeader :title="t('fleet.title')" :description="t('fleet.description')">
      <template #actions>
        <button
          type="button"
          class="btn btn-primary btn-sm"
          :disabled="!workspace.selectedID"
          @click="createOpen = true"
        >
          <Add16 aria-hidden="true" />
          {{ t('fleet.create') }}
        </button>
        <button type="button" class="btn btn-ghost btn-sm" :disabled="loading" @click="load">
          <Renew16 aria-hidden="true" />
          {{ t('common.refresh') }}
        </button>
      </template>
    </PageHeader>

    <p v-if="workspace.error" class="note note-error mb-4">{{ workspace.error }}</p>
    <p v-else-if="error" class="note note-error mb-4">{{ error }}</p>

    <div v-if="!workspace.loading && !workspace.list.length" class="plate px-5 py-8">
      <h2 class="title text-base text-base-content">{{ t('app.noWorkspaces') }}</h2>
      <p class="voice mt-2 max-w-xl">{{ t('app.noWorkspacesBody') }}</p>
    </div>

    <template v-else>
      <div class="mb-6 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <ConsolePanel :title="t('fleet.maxDaemons')">
          <p class="numeric text-lg text-base-content">
            {{ daemonCount }} <span class="text-faint">/ {{ limit || '—' }}</span>
          </p>
        </ConsolePanel>
        <ConsolePanel :title="t('fleet.retention')">
          <p class="numeric text-lg text-base-content">
            {{ quota.metrics_retention_days ? t('fleet.days', { count: quota.metrics_retention_days }) : '—' }}
          </p>
        </ConsolePanel>
        <ConsolePanel :title="t('fleet.pollInterval')">
          <p class="numeric text-lg text-base-content">
            {{ quota.polling_interval_seconds ? t('fleet.seconds', { count: quota.polling_interval_seconds }) : '—' }}
          </p>
        </ConsolePanel>
        <ConsolePanel :title="t('fleet.maxTerminalSessions')">
          <p class="numeric text-lg text-base-content">{{ quota.max_terminal_sessions ?? '—' }}</p>
        </ConsolePanel>
      </div>

      <ConsolePanel :title="t('fleet.daemons')" :note="String(daemons.length)" flush>
        <LoadingSkeleton v-if="loading" :rows="4" :columns="[3, 2, 2, 1]" class="px-4 py-4 sm:px-6" />
        <div v-else-if="!daemons.length" class="px-4 py-8 sm:px-6">
          <h2 class="title text-base text-base-content">{{ t('fleet.empty') }}</h2>
          <p class="voice mt-2 max-w-xl">{{ t('fleet.emptyBody') }}</p>
        </div>
        <div v-else class="px-4 py-4 sm:px-6">
          <table class="ledger ledger-cards">
            <thead>
              <tr>
                <th scope="col">{{ t('common.name') }}</th>
                <th scope="col">{{ t('fleet.status') }}</th>
                <th scope="col">{{ t('fleet.host') }}</th>
                <th scope="col">{{ t('fleet.lastSeen') }}</th>
                <th scope="col"><span class="sr-only">{{ t('common.actions') }}</span></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="daemon in daemons" :key="daemon.id">
                <td :data-label="t('common.name')">
                  <div class="cell-stack">
                    <RouterLink :to="`/daemons/${daemon.id}`" class="font-semibold text-base-content hover:underline">
                      {{ daemon.name }}
                    </RouterLink>
                    <span class="font-mono text-2xs text-faint" :title="daemon.id">{{ daemon.id }}</span>
                  </div>
                </td>
                <td :data-label="t('fleet.status')">
                  <StatusPill :tone="daemonTone(daemon)" :label="daemonState(daemon)" />
                </td>
                <td :data-label="t('fleet.host')">
                  <span class="font-mono text-2xs text-dim" :title="daemon.host_id">{{ daemon.host_id || '—' }}</span>
                </td>
                <td :data-label="t('fleet.lastSeen')">
                  <span :title="stamp(daemon.last_seen_at)">{{ relative(daemon.last_seen_at) }}</span>
                </td>
                <td>
                  <RouterLink
                    :to="`/daemons/${daemon.id}`"
                    class="btn btn-ghost btn-xs"
                    :aria-label="t('fleet.open')"
                    :title="t('fleet.open')"
                  >
                    <ChevronRight16 aria-hidden="true" />
                  </RouterLink>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </ConsolePanel>
    </template>

    <dialog v-if="createOpen" class="modal modal-open" aria-modal="true" :aria-label="t('fleet.createTitle')">
      <form class="modal-box sheet max-w-md" @submit.prevent="create">
        <div class="px-5 py-4">
          <h2 class="title text-base text-base-content">{{ t('fleet.createTitle') }}</h2>
          <p class="voice mt-2">{{ t('fleet.createBody') }}</p>
          <label class="field-label mt-5" for="daemon-name">{{ t('fleet.nameLabel') }}</label>
          <input
            id="daemon-name"
            v-model="createName"
            class="input"
            :placeholder="t('fleet.namePlaceholder')"
            autocomplete="off"
            required
          />
        </div>
        <footer class="flex items-center justify-end gap-2 bg-base-200/50 px-5 py-3">
          <button type="button" class="btn btn-ghost btn-sm" @click="createOpen = false">
            {{ t('common.cancel') }}
          </button>
          <button type="submit" class="btn btn-primary btn-sm" :disabled="creating || !createName.trim()">
            {{ creating ? t('common.working') : t('fleet.create') }}
          </button>
        </footer>
      </form>
      <button type="button" class="modal-backdrop bg-black/60" :aria-label="t('common.close')" @click="createOpen = false" />
    </dialog>

    <dialog v-if="created" class="modal modal-open" aria-modal="true" :aria-label="t('fleet.secretTitle')">
      <div class="modal-box sheet max-w-lg">
        <div class="px-5 py-4">
          <h2 class="title flex items-center gap-2 text-base text-base-content">
            <Cloud16 aria-hidden="true" />
            {{ t('fleet.secretTitle') }}
          </h2>
          <p class="voice mt-2">{{ t('fleet.secretBody') }}</p>
          <div class="well mt-4 flex items-center gap-3 px-4 py-3">
            <code class="min-w-0 flex-1 break-all font-mono text-xs text-base-content">{{ created.secret }}</code>
            <CopyButton :value="created.secret" />
          </div>
          <p class="field-hint">{{ t('fleet.secretHint') }}</p>
        </div>
        <footer class="flex items-center justify-end bg-base-200/50 px-5 py-3">
          <button type="button" class="btn btn-primary btn-sm" @click="created = null">
            {{ t('common.close') }}
          </button>
        </footer>
      </div>
    </dialog>
  </SessionGate>
</template>
