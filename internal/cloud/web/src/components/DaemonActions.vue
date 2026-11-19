<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Play16, Renew16 } from '@carbon/icons-vue'

import type { Action, WebhookResult } from '@/lib/api/types'
import { CloudError } from '@/lib/api/client'
import { daemonApi } from '@/lib/api'
import { stamp } from '@/lib/format'
import ConsolePanel from '@/components/ConsolePanel.vue'
import LoadingSkeleton from '@/components/LoadingSkeleton.vue'
import { useNoticeStore } from '@/stores/notice'
import IconButton from '@/components/IconButton.vue'
import StatusPill from '@/components/StatusPill.vue'
import ConfirmAction from '@/components/ConfirmAction.vue'

const props = defineProps<{ daemonId: string }>()
const notice = useNoticeStore()
const { t } = useI18n()

const actions = ref<Action[]>([])
const loading = ref(true)
const error = ref('')
const pending = ref<Action | null>(null)
const running = ref('')
const result = ref<WebhookResult | null>(null)
const resultFor = ref('')
let disposed = false
// The API carries the daemon's stdout/stderr as base64 so arbitrary bytes
// survive the relay; the console decodes it back to UTF-8 for display.
const decodedResult = computed(() => {
  const raw = result.value?.result_body
  if (!raw) return ''
  try {
    const binary = atob(raw)
    const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0))
    return new TextDecoder().decode(bytes)
  } catch {
    return raw
  }
})

async function load() {
  loading.value = true
  error.value = ''
  try {
    actions.value = await daemonApi.actions(props.daemonId)
  } catch (caught) {
    error.value = caught instanceof CloudError ? caught.message : t('api.requestFailed')
  } finally {
    loading.value = false
  }
}

async function run() {
  const action = pending.value
  if (!action) return
  pending.value = null
  error.value = ''
  running.value = action.name
  result.value = null
  resultFor.value = action.name
  try {
    const queued = await daemonApi.run(props.daemonId, action.name)
    notice.ok(t('daemon.runStarted', { name: action.display_name || action.name }))
    for (let attempt = 0; attempt < 40; attempt++) {
      await new Promise((resolve) => window.setTimeout(resolve, 1500))
      if (disposed) return
      const current = await daemonApi.runResult(props.daemonId, queued.id)
      result.value = current
      if (current.status === 'done') break
    }
  } catch (caught) {
    error.value = caught instanceof Error ? caught.message : t('daemon.runFailed')
  } finally {
    running.value = ''
  }
}


onBeforeUnmount(() => {
  disposed = true
})
onMounted(load)
</script>

<template>
  <div class="space-y-5">
    <ConsolePanel :title="t('daemon.actionsTitle')" :note="String(actions.length)" flush>
      <template #header>
        <IconButton :label="t('common.refresh')" :disabled="loading" @click="load">
          <Renew16 aria-hidden="true" />
        </IconButton>
      </template>
      <p v-if="error" class="note note-error m-4">{{ error }}</p>
      <LoadingSkeleton v-if="loading" :rows="4" :columns="[3, 3, 1, 1]" class="px-4 py-4 sm:px-6" />
      <div v-else-if="!actions.length" class="px-4 py-8 sm:px-6">
        <p class="voice">{{ t('daemon.actionsEmpty') }}</p>
      </div>
      <div v-else class="px-4 py-4 sm:px-6">
        <table class="ledger ledger-cards">
          <thead>
            <tr>
              <th scope="col">{{ t('daemon.actionName') }}</th>
              <th scope="col">{{ t('daemon.actionDisplay') }}</th>
              <th scope="col">{{ t('common.enabled') }}</th>
              <th scope="col">{{ t('daemon.actionNotifySuccess') }}</th>
              <th scope="col">{{ t('daemon.actionNotifyFailure') }}</th>
              <th scope="col">{{ t('daemon.actionUpdated') }}</th>
              <th scope="col"><span class="sr-only">{{ t('common.actions') }}</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="action in actions" :key="action.name">
              <td :data-label="t('daemon.actionName')">
                <span class="font-mono text-xs text-base-content">{{ action.name }}</span>
              </td>
              <td :data-label="t('daemon.actionDisplay')">{{ action.display_name || '—' }}</td>
              <td :data-label="t('common.enabled')">
                <StatusPill
                  :tone="action.enabled ? 'success' : 'neutral'"
                  :label="action.enabled ? t('common.enabled') : t('common.disabled')"
                />
              </td>
              <td :data-label="t('daemon.actionNotifySuccess')">{{ action.notify_on_success ? t('common.yes') : t('common.no') }}</td>
              <td :data-label="t('daemon.actionNotifyFailure')">{{ action.notify_on_failure ? t('common.yes') : t('common.no') }}</td>
              <td :data-label="t('daemon.actionUpdated')">{{ stamp(action.updated_at) }}</td>
              <td>
                <button
                  type="button"
                  class="btn btn-ghost btn-xs"
                  :disabled="!action.enabled || running !== ''"
                  @click="pending = action"
                >
                  <Play16 aria-hidden="true" />
                  {{ running === action.name ? t('common.running') : t('common.run') }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </ConsolePanel>

    <ConsolePanel v-if="resultFor" :title="t('daemon.runResult')" :note="resultFor">
      <template v-if="result">
        <div class="flex flex-wrap items-center gap-2">
          <StatusPill
            :tone="result.status === 'done' ? 'success' : 'info'"
            :label="result.status"
          />
          <span v-if="result.status === 'done'" class="text-2xs text-dim">
            {{ result.result_code ? t('daemon.runCompleted', { code: result.result_code }) : t('daemon.runSuccess') }}
          </span>
        </div>
        <p v-if="result.result_error" class="note note-error mt-3">{{ result.result_error }}</p>
        <pre v-if="decodedResult" class="well mt-3 max-h-64 overflow-auto whitespace-pre-wrap break-all px-3 py-2 font-mono text-2xs text-dim">{{ decodedResult }}</pre>
        <p v-if="result.status !== 'done'" class="field-hint">{{ t('daemon.runPending') }}</p>
      </template>
      <p v-else class="voice">{{ running ? t('daemon.runPending') : t('common.loading') }}</p>
    </ConsolePanel>

    <ConfirmAction
      v-if="pending"
      :title="t('common.run')"
      :body="`${pending.display_name || pending.name}. ${t('daemon.runBody')}`"
      :action="t('common.run')"
      @cancel="pending = null"
      @confirm="run"
    />
  </div>
</template>
