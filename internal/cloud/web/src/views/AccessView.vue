<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Add16, Key16, Renew16, TrashCan16 } from '@carbon/icons-vue'

import type { Credential, CredentialToken } from '@/lib/api/types'
import { CloudError } from '@/lib/api/client'
import { credentialApi } from '@/lib/api'
import { relative, stamp } from '@/lib/format'
import { useNoticeStore } from '@/stores/notice'
import SessionGate from '@/components/SessionGate.vue'
import PageHeader from '@/components/PageHeader.vue'
import ConsolePanel from '@/components/ConsolePanel.vue'
import LoadingSkeleton from '@/components/LoadingSkeleton.vue'
import IconButton from '@/components/IconButton.vue'
import CopyButton from '@/components/CopyButton.vue'
import ConfirmAction from '@/components/ConfirmAction.vue'

const { t } = useI18n()
const notice = useNoticeStore()

const credentials = ref<Credential[]>([])
const loading = ref(false)
const error = ref('')
const createOpen = ref(false)
const creating = ref(false)
const created = ref<CredentialToken | null>(null)
const removing = ref<Credential | null>(null)
const label = ref('')
const daemonIDs = ref('')
const hostIDs = ref('')
const actionNames = ref('')

function split(value: string): string[] {
  return value
    .split(',')
    .map((part) => part.trim())
    .filter(Boolean)
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    credentials.value = await credentialApi.list()
  } catch (caught) {
    error.value = caught instanceof CloudError ? caught.message : t('api.requestFailed')
  } finally {
    loading.value = false
  }
}

async function create() {
  if (!label.value.trim()) return
  creating.value = true
  try {
    created.value = await credentialApi.create({
      label: label.value.trim(),
      daemon_ids: split(daemonIDs.value),
      host_ids: split(hostIDs.value),
      action_names: split(actionNames.value),
    })
    createOpen.value = false
    label.value = ''
    daemonIDs.value = ''
    hostIDs.value = ''
    actionNames.value = ''
    await load()
  } catch (caught) {
    notice.fail(caught instanceof Error ? caught.message : t('api.requestFailed'))
  } finally {
    creating.value = false
  }
}

async function remove() {
  if (!removing.value) return
  try {
    await credentialApi.remove(removing.value.id)
    removing.value = null
    notice.ok(t('common.delete'))
    await load()
  } catch (caught) {
    notice.fail(caught instanceof Error ? caught.message : t('api.requestFailed'))
  }
}

onMounted(load)
</script>

<template>
  <SessionGate>
    <PageHeader :title="t('access.title')" :description="t('access.description')">
      <template #actions>
        <button type="button" class="btn btn-primary btn-sm" @click="createOpen = true">
          <Add16 aria-hidden="true" />
          {{ t('access.create') }}
        </button>
        <button type="button" class="btn btn-ghost btn-sm" :disabled="loading" @click="load">
          <Renew16 aria-hidden="true" />
          {{ t('common.refresh') }}
        </button>
      </template>
    </PageHeader>

    <p v-if="error" class="note note-error mb-4">{{ error }}</p>

    <ConsolePanel :title="t('access.title')" :note="String(credentials.length)" flush>
      <LoadingSkeleton v-if="loading" :rows="4" :columns="[3, 3, 2, 1]" class="px-4 py-4 sm:px-6" />
      <div v-else-if="!credentials.length" class="px-4 py-8 sm:px-6">
        <h2 class="title text-base text-base-content">{{ t('access.empty') }}</h2>
        <p class="voice mt-2 max-w-xl">{{ t('access.emptyBody') }}</p>
      </div>
      <div v-else class="px-4 py-4 sm:px-6">
        <table class="ledger ledger-cards">
          <thead>
            <tr>
              <th scope="col">{{ t('access.label') }}</th>
              <th scope="col">{{ t('access.daemonScope') }}</th>
              <th scope="col">{{ t('access.actionScope') }}</th>
              <th scope="col">{{ t('access.lastUsed') }}</th>
              <th scope="col">{{ t('common.created') }}</th>
              <th scope="col"><span class="sr-only">{{ t('common.actions') }}</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="credential in credentials" :key="credential.id">
              <td :data-label="t('access.label')">
                <div class="cell-stack">
                  <span class="flex items-center gap-1.5 font-semibold text-base-content">
                    <Key16 aria-hidden="true" />
                    {{ credential.label }}
                  </span>
                  <span class="font-mono text-2xs text-faint">{{ credential.id }}</span>
                </div>
              </td>
              <td :data-label="t('access.daemonScope')">
                <span class="font-mono text-2xs text-dim">{{ credential.daemon_ids.join(', ') || t('common.all') }}</span>
              </td>
              <td :data-label="t('access.actionScope')">
                <span class="font-mono text-2xs text-dim">{{ credential.action_names.join(', ') || t('common.all') }}</span>
              </td>
              <td :data-label="t('access.lastUsed')" :title="stamp(credential.last_used_at)">
                {{ credential.last_used_at ? relative(credential.last_used_at) : t('access.neverUsed') }}
              </td>
              <td :data-label="t('common.created')" :title="stamp(credential.created_at)">
                {{ relative(credential.created_at) }}
              </td>
              <td>
                <IconButton :label="t('access.delete')" tone="danger" @click="removing = credential">
                  <TrashCan16 aria-hidden="true" />
                </IconButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </ConsolePanel>

    <dialog v-if="createOpen" class="modal modal-open" aria-modal="true" :aria-label="t('access.createTitle')">
      <form class="modal-box sheet max-w-lg" @submit.prevent="create">
        <div class="px-5 py-4">
          <h2 class="title text-base text-base-content">{{ t('access.createTitle') }}</h2>
          <label class="field-label mt-4" for="credential-label">{{ t('access.label') }}</label>
          <input
            id="credential-label"
            v-model="label"
            class="input"
            :placeholder="t('access.labelPlaceholder')"
            required
          />
          <label class="field-label mt-4" for="credential-daemons">{{ t('access.daemonScope') }}</label>
          <input
            id="credential-daemons"
            v-model="daemonIDs"
            class="input font-mono"
            placeholder="daemon-id-a, daemon-id-b"
          />
          <label class="field-label mt-4" for="credential-hosts">{{ t('access.hostScope') }}</label>
          <input id="credential-hosts" v-model="hostIDs" class="input font-mono" placeholder="host-id" />
          <label class="field-label mt-4" for="credential-actions">{{ t('access.actionScope') }}</label>
          <input
            id="credential-actions"
            v-model="actionNames"
            class="input font-mono"
            placeholder="backup, deploy"
          />
          <p class="field-hint">{{ t('access.scopeHint') }}</p>
        </div>
        <footer class="flex items-center justify-end gap-2 bg-base-200/50 px-5 py-3">
          <button type="button" class="btn btn-ghost btn-sm" @click="createOpen = false">
            {{ t('common.cancel') }}
          </button>
          <button type="submit" class="btn btn-primary btn-sm" :disabled="creating || !label.trim()">
            {{ creating ? t('common.working') : t('access.create') }}
          </button>
        </footer>
      </form>
      <button type="button" class="modal-backdrop bg-black/60" :aria-label="t('common.close')" @click="createOpen = false" />
    </dialog>

    <dialog v-if="created" class="modal modal-open" aria-modal="true" :aria-label="t('access.tokenTitle')">
      <div class="modal-box sheet max-w-lg">
        <div class="px-5 py-4">
          <h2 class="title text-base text-base-content">{{ t('access.tokenTitle') }}</h2>
          <p class="voice mt-2">{{ t('access.tokenBody') }}</p>
          <div class="well mt-4 flex items-center gap-3 px-4 py-3">
            <code class="min-w-0 flex-1 break-all font-mono text-xs text-base-content">{{ created.token }}</code>
            <CopyButton :value="created.token" />
          </div>
        </div>
        <footer class="flex items-center justify-end bg-base-200/50 px-5 py-3">
          <button type="button" class="btn btn-primary btn-sm" @click="created = null">
            {{ t('common.close') }}
          </button>
        </footer>
      </div>
    </dialog>

    <ConfirmAction
      v-if="removing"
      :title="t('access.deleteTitle')"
      :body="t('access.deleteBody')"
      :action="t('access.delete')"
      tone="error"
      @cancel="removing = null"
      @confirm="remove"
    />
  </SessionGate>
</template>
