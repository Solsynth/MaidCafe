<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import { Edit16, Renew16, TrashCan16 } from '@carbon/icons-vue'

import type { Daemon } from '@/lib/api/types'
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
import ConfirmAction from '@/components/ConfirmAction.vue'
import DaemonOverview from '@/components/DaemonOverview.vue'
import DaemonContainers from '@/components/DaemonContainers.vue'
import DaemonLogs from '@/components/DaemonLogs.vue'
import DaemonActions from '@/components/DaemonActions.vue'
import NotificationPreferenceEditor from '@/components/NotificationPreferenceEditor.vue'

const { t } = useI18n()
const route = useRoute()
const workspace = useWorkspaceStore()
const notice = useNoticeStore()

const daemon = ref<Daemon | null>(null)
const loading = ref(true)
const error = ref('')
const tab = ref<'overview' | 'containers' | 'logs' | 'actions' | 'notifications'>('overview')
const renameOpen = ref(false)
const renameValue = ref('')
const rotateOpen = ref(false)
const disableOpen = ref(false)
const secret = ref('')
const busy = ref(false)

const daemonID = computed(() => String(route.params.id ?? ''))
const tabs = computed(() => [
  { key: 'overview', label: t('daemon.overview') },
  { key: 'containers', label: t('daemon.containers') },
  { key: 'logs', label: t('daemon.logs') },
  { key: 'actions', label: t('daemon.actions') },
  { key: 'notifications', label: t('daemon.notifications') },
] as const)

const live = computed(() => {
  if (!daemon.value?.enabled || !daemon.value.last_seen_at) return false
  return Date.now() - new Date(daemon.value.last_seen_at).getTime() < 5 * 60 * 1000
})

async function load() {
  loading.value = true
  error.value = ''
  try {
    daemon.value = await daemonApi.get(daemonID.value)
    renameValue.value = daemon.value.name
  } catch (caught) {
    error.value = caught instanceof CloudError ? caught.message : t('api.requestFailed')
  } finally {
    loading.value = false
  }
}

async function rename() {
  const name = renameValue.value.trim()
  if (!name) return
  busy.value = true
  try {
    daemon.value = await daemonApi.update(daemonID.value, { name })
    renameOpen.value = false
  } catch (caught) {
    notice.fail(caught instanceof Error ? caught.message : t('api.requestFailed'))
  } finally {
    busy.value = false
  }
}

async function patch(patch: { enabled?: boolean; terminal_relay_enabled?: boolean }) {
  busy.value = true
  try {
    daemon.value = await daemonApi.update(daemonID.value, patch)
  } catch (caught) {
    notice.fail(caught instanceof Error ? caught.message : t('api.requestFailed'))
  } finally {
    busy.value = false
  }
}

async function rotate() {
  busy.value = true
  try {
    const response = await daemonApi.rotateSecret(daemonID.value)
    secret.value = response.secret
    rotateOpen.value = false
  } catch (caught) {
    notice.fail(caught instanceof Error ? caught.message : t('api.requestFailed'))
  } finally {
    busy.value = false
  }
}

async function disable() {
  busy.value = true
  try {
    await daemonApi.disable(daemonID.value)
    disableOpen.value = false
    await load()
  } catch (caught) {
    notice.fail(caught instanceof Error ? caught.message : t('api.requestFailed'))
  } finally {
    busy.value = false
  }
}

onMounted(async () => {
  await workspace.load()
  await load()
})
</script>

<template>
  <SessionGate>
    <p v-if="error" class="note note-error">{{ error }}</p>

    <LoadingSkeleton v-if="loading" :rows="5" variant="rows" />

    <template v-else-if="daemon">
      <PageHeader :title="daemon.name" :description="`${daemon.host_id || daemon.id}`">
        <template #actions>
          <StatusPill
            :tone="!daemon.enabled ? 'neutral' : live ? 'success' : 'error'"
            :label="!daemon.enabled ? t('fleet.disabled') : live ? t('fleet.live') : t('fleet.stale')"
          />
          <button type="button" class="btn btn-ghost btn-sm" :disabled="busy" @click="renameOpen = true">
            <Edit16 aria-hidden="true" />
            {{ t('daemon.rename') }}
          </button>
          <button type="button" class="btn btn-ghost btn-sm" :disabled="busy" @click="rotateOpen = true">
            <Renew16 aria-hidden="true" />
            {{ t('daemon.rotate') }}
          </button>
          <button type="button" class="btn btn-error btn-sm" :disabled="busy || !daemon.enabled" @click="disableOpen = true">
            <TrashCan16 aria-hidden="true" />
            {{ t('daemon.disable') }}
          </button>
        </template>
      </PageHeader>

      <nav class="mb-6 flex w-fit max-w-full gap-1 overflow-x-auto rounded-control bg-base-200/60 p-1" :aria-label="t('daemon.title')">
        <button
          v-for="item in tabs"
          :key="item.key"
          type="button"
          class="whitespace-nowrap rounded-control px-3 py-1.5 text-2xs tracking-[0.02em] transition-colors"
          :class="tab === item.key ? 'bg-base-300 text-base-content' : 'text-faint hover:bg-base-300/50 hover:text-dim'"
          :aria-pressed="tab === item.key"
          @click="tab = item.key"
        >
          {{ item.label }}
        </button>
      </nav>

      <div v-if="tab === 'overview'" class="space-y-5">
        <ConsolePanel :title="t('daemon.identity')">
          <dl class="grid gap-x-6 gap-y-3 text-xs sm:grid-cols-2 lg:grid-cols-3">
            <div>
              <dt class="eyebrow">{{ t('daemon.daemonId') }}</dt>
              <dd class="mt-1 break-all font-mono text-dim">{{ daemon.id }}</dd>
            </div>
            <div>
              <dt class="eyebrow">{{ t('daemon.workspace') }}</dt>
              <dd class="mt-1 text-dim">{{ workspace.selected?.name || daemon.workspace_id }}</dd>
            </div>
            <div>
              <dt class="eyebrow">{{ t('daemon.hostId') }}</dt>
              <dd class="mt-1 break-all font-mono text-dim">{{ daemon.host_id || '—' }}</dd>
            </div>
            <div>
              <dt class="eyebrow">{{ t('daemon.lastSeen') }}</dt>
              <dd class="mt-1 text-dim" :title="stamp(daemon.last_seen_at)">
                {{ relative(daemon.last_seen_at) }}
              </dd>
            </div>
            <div>
              <dt class="eyebrow">{{ t('daemon.created') }}</dt>
              <dd class="mt-1 text-dim">{{ stamp(daemon.created_at) }}</dd>
            </div>
            <div>
              <dt class="eyebrow">{{ t('daemon.disconnected') }}</dt>
              <dd class="mt-1 text-dim">{{ stamp(daemon.disconnected_at) }}</dd>
            </div>
          </dl>

          <div class="mt-5 flex flex-wrap items-center gap-4 border-t border-line-soft pt-4">
            <label class="flex items-center gap-2 text-xs text-dim">
              <input
                type="checkbox"
                role="switch"
                class="switch"
                :checked="daemon.enabled"
                :disabled="busy"
                @change="patch({ enabled: ($event.target as HTMLInputElement).checked })"
              />
              {{ t('daemon.enabled') }}
            </label>
            <label class="flex items-center gap-2 text-xs text-dim">
              <input
                type="checkbox"
                role="switch"
                class="switch"
                :checked="daemon.terminal_relay_enabled"
                :disabled="busy"
                @change="patch({ terminal_relay_enabled: ($event.target as HTMLInputElement).checked })"
              />
              {{ t('daemon.terminalRelay') }}
            </label>
          </div>
          <p class="field-hint">{{ t('daemon.enabledHint') }} {{ t('daemon.terminalRelayHint') }}</p>
        </ConsolePanel>

        <DaemonOverview :daemon-id="daemon.id" />
      </div>

      <DaemonContainers v-else-if="tab === 'containers'" :daemon-id="daemon.id" />
      <DaemonLogs v-else-if="tab === 'logs'" :daemon-id="daemon.id" />
      <DaemonActions v-else-if="tab === 'actions'" :daemon-id="daemon.id" />
      <NotificationPreferenceEditor
        v-else-if="tab === 'notifications'"
        :workspace-id="workspace.selected?.id ?? daemon.workspace_id"
        :daemon-id="daemon.id"
      />
    </template>

    <dialog v-if="renameOpen" class="modal modal-open" aria-modal="true" :aria-label="t('daemon.renameTitle')">
      <form class="modal-box sheet max-w-md" @submit.prevent="rename">
        <div class="px-5 py-4">
          <h2 class="title text-base text-base-content">{{ t('daemon.renameTitle') }}</h2>
          <label class="field-label mt-4" for="daemon-rename">{{ t('daemon.name') }}</label>
          <input id="daemon-rename" v-model="renameValue" class="input" required />
        </div>
        <footer class="flex items-center justify-end gap-2 bg-base-200/50 px-5 py-3">
          <button type="button" class="btn btn-ghost btn-sm" @click="renameOpen = false">{{ t('common.cancel') }}</button>
          <button type="submit" class="btn btn-primary btn-sm" :disabled="busy || !renameValue.trim()">
            {{ busy ? t('common.saving') : t('common.save') }}
          </button>
        </footer>
      </form>
      <button type="button" class="modal-backdrop bg-black/60" :aria-label="t('common.close')" @click="renameOpen = false" />
    </dialog>

    <dialog v-if="secret" class="modal modal-open" aria-modal="true" :aria-label="t('daemon.secretTitle')">
      <div class="modal-box sheet max-w-lg">
        <div class="px-5 py-4">
          <h2 class="title text-base text-base-content">{{ t('daemon.secretTitle') }}</h2>
          <p class="voice mt-2">{{ t('daemon.secretBody') }}</p>
          <div class="well mt-4 flex items-center gap-3 px-4 py-3">
            <code class="min-w-0 flex-1 break-all font-mono text-xs text-base-content">{{ secret }}</code>
            <CopyButton :value="secret" />
          </div>
        </div>
        <footer class="flex items-center justify-end bg-base-200/50 px-5 py-3">
          <button type="button" class="btn btn-primary btn-sm" @click="secret = ''">{{ t('common.close') }}</button>
        </footer>
      </div>
    </dialog>

    <ConfirmAction
      v-if="rotateOpen"
      :title="t('daemon.rotateTitle')"
      :body="t('daemon.rotateBody')"
      :action="t('daemon.rotate')"
      :busy="busy"
      @cancel="rotateOpen = false"
      @confirm="rotate"
    />
    <ConfirmAction
      v-if="disableOpen"
      :title="t('daemon.disableTitle')"
      :body="t('daemon.disableBody')"
      :action="t('daemon.disable')"
      tone="error"
      :busy="busy"
      @cancel="disableOpen = false"
      @confirm="disable"
    />
  </SessionGate>
</template>
