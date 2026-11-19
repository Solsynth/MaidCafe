<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Checkmark16, Renew16 } from '@carbon/icons-vue'

import type { Notification } from '@/lib/api/types'
import { CloudError } from '@/lib/api/client'
import { notificationApi } from '@/lib/api'
import { relative, stamp } from '@/lib/format'
import { useWorkspaceStore } from '@/stores/workspace'
import { useNoticeStore } from '@/stores/notice'
import SessionGate from '@/components/SessionGate.vue'
import PageHeader from '@/components/PageHeader.vue'
import ConsolePanel from '@/components/ConsolePanel.vue'
import LoadingSkeleton from '@/components/LoadingSkeleton.vue'
import IconButton from '@/components/IconButton.vue'
import StatusPill from '@/components/StatusPill.vue'

const { t } = useI18n()
const workspace = useWorkspaceStore()
const notice = useNoticeStore()

const items = ref<Notification[]>([])
const unreadOnly = ref(false)
const loading = ref(false)
const error = ref('')

async function load() {
  if (!workspace.selectedID) {
    items.value = []
    return
  }
  loading.value = true
  error.value = ''
  try {
    items.value = await notificationApi.list(workspace.selectedID, {
      unread: unreadOnly.value || undefined,
      limit: 100,
    })
  } catch (caught) {
    items.value = []
    error.value = caught instanceof CloudError ? caught.message : t('api.requestFailed')
  } finally {
    loading.value = false
  }
}

async function markRead(item: Notification) {
  try {
    await notificationApi.markRead(item.id)
    item.read_at = new Date().toISOString()
  } catch (caught) {
    notice.fail(caught instanceof Error ? caught.message : t('api.requestFailed'))
  }
}

async function markAll() {
  if (!workspace.selectedID) return
  try {
    await notificationApi.markAllRead(workspace.selectedID)
    notice.ok(t('notifications.markAll'))
    await load()
  } catch (caught) {
    notice.fail(caught instanceof Error ? caught.message : t('api.requestFailed'))
  }
}

onMounted(async () => {
  await workspace.load()
  await load()
})

watch([() => workspace.selectedID, unreadOnly], () => void load())
</script>

<template>
  <SessionGate>
    <PageHeader :title="t('notifications.title')" :description="t('notifications.description')">
      <template #actions>
        <button type="button" class="btn btn-ghost btn-sm" :disabled="loading" @click="markAll">
          <Checkmark16 aria-hidden="true" />
          {{ t('notifications.markAll') }}
        </button>
        <button type="button" class="btn btn-ghost btn-sm" :disabled="loading" @click="load">
          <Renew16 aria-hidden="true" />
          {{ t('common.refresh') }}
        </button>
      </template>
    </PageHeader>

    <p v-if="error" class="note note-error mb-4">{{ error }}</p>

    <ConsolePanel :title="t('notifications.title')" :note="String(items.length)" flush>
      <template #header>
        <label class="flex items-center gap-1.5 text-2xs text-dim">
          <input v-model="unreadOnly" type="checkbox" class="checkbox checkbox-xs" />
          {{ t('notifications.unreadOnly') }}
        </label>
      </template>

      <LoadingSkeleton v-if="loading" :rows="5" :columns="[2, 3, 2, 1]" class="px-4 py-4 sm:px-6" />
      <div v-else-if="!items.length" class="px-4 py-8 sm:px-6">
        <h2 class="title text-base text-base-content">{{ t('notifications.empty') }}</h2>
        <p class="voice mt-2 max-w-xl">{{ t('notifications.emptyBody') }}</p>
      </div>
      <div v-else class="px-4 py-4 sm:px-6">
        <table class="ledger ledger-cards">
          <thead>
            <tr>
              <th scope="col">{{ t('notifications.kind') }}</th>
              <th scope="col">{{ t('common.name') }}</th>
              <th scope="col">{{ t('notifications.daemon') }}</th>
              <th scope="col">{{ t('common.created') }}</th>
              <th scope="col"><span class="sr-only">{{ t('common.actions') }}</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in items" :key="item.id">
              <td :data-label="t('notifications.kind')">
                <div class="cell-stack">
                  <StatusPill
                    :tone="item.read_at ? 'neutral' : 'info'"
                    :label="item.read_at ? t('notifications.read') : t('notifications.unread')"
                  />
                  <span class="mt-1 font-mono text-2xs text-faint">{{ item.kind }}</span>
                </div>
              </td>
              <td :data-label="t('common.name')">
                <div class="cell-stack">
                  <span class="font-semibold text-base-content">{{ item.title }}</span>
                  <span v-if="item.subtitle" class="text-2xs text-dim">{{ item.subtitle }}</span>
                  <span class="mt-1 whitespace-pre-wrap break-words text-xs text-dim">{{ item.body }}</span>
                </div>
              </td>
              <td :data-label="t('notifications.daemon')">
                <RouterLink
                  v-if="item.daemon_id"
                  :to="`/daemons/${item.daemon_id}`"
                  class="font-mono text-2xs text-dim hover:text-base-content"
                >
                  {{ item.daemon_id }}
                </RouterLink>
                <span v-else class="text-2xs text-faint">{{ t('notifications.system') }}</span>
              </td>
              <td :data-label="t('common.created')" :title="stamp(item.created_at)">
                {{ relative(item.created_at) }}
              </td>
              <td>
                <IconButton
                  v-if="!item.read_at"
                  :label="t('notifications.markRead')"
                  @click="markRead(item)"
                >
                  <Checkmark16 aria-hidden="true" />
                </IconButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </ConsolePanel>
  </SessionGate>
</template>
