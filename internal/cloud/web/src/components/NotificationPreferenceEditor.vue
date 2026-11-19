<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Renew16 } from '@carbon/icons-vue'

import type { NotificationPreference, NotificationTopic } from '@/lib/api/types'
import { CloudError } from '@/lib/api/client'
import { notificationApi } from '@/lib/api'
import { useNoticeStore } from '@/stores/notice'
import ConsolePanel from '@/components/ConsolePanel.vue'
import LoadingSkeleton from '@/components/LoadingSkeleton.vue'
import IconButton from '@/components/IconButton.vue'

const props = defineProps<{
  workspaceId: string
  daemonId?: string
}>()

const { t } = useI18n()
const notice = useNoticeStore()

const topics = ref<NotificationTopic[]>([])
const preferences = ref<NotificationPreference[]>([])
const loading = ref(true)
const saving = ref('')
const error = ref('')

const byTopic = computed(() => new Map(preferences.value.map((row) => [row.topic, row])))
const isDaemon = computed(() => Boolean(props.daemonId))

function valueFor(topic: string): string {
  const row = byTopic.value.get(topic)
  if (!row) return isDaemon.value ? 'inherit' : '0'
  return String(row.preference)
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [topicList, current] = await Promise.all([
      notificationApi.topics(props.workspaceId),
      isDaemon.value
        ? notificationApi.daemonPreferences(props.daemonId ?? '', props.workspaceId)
        : notificationApi
            .preferences(props.workspaceId)
            .then((rows) => rows.filter((row) => !row.daemon_id)),
    ])
    topics.value = topicList
    preferences.value = current
  } catch (caught) {
    error.value = caught instanceof CloudError ? caught.message : t('api.requestFailed')
  } finally {
    loading.value = false
  }
}

async function change(topic: string, event: Event) {
  const value = (event.target as HTMLSelectElement).value
  saving.value = topic
  try {
    if (isDaemon.value) {
      if (value === 'inherit') {
        await notificationApi.removeDaemonPreference(props.daemonId ?? '', topic, props.workspaceId)
      } else {
        await notificationApi.setDaemonPreference(props.daemonId ?? '', topic, Number(value), props.workspaceId)
      }
    } else {
      await notificationApi.setPreference(topic, Number(value), props.workspaceId)
    }
    notice.ok(t('settings.saved'))
    await load()
  } catch (caught) {
    notice.fail(caught instanceof Error ? caught.message : t('api.requestFailed'))
  } finally {
    saving.value = ''
  }
}

onMounted(load)
</script>

<template>
  <ConsolePanel :title="isDaemon ? t('daemon.notificationsTitle') : t('settings.topicsTitle')" flush>
    <template #header>
      <IconButton :label="t('common.refresh')" :disabled="loading" @click="load">
        <Renew16 aria-hidden="true" />
      </IconButton>
    </template>
    <p v-if="isDaemon" class="voice px-4 pt-3 sm:px-6">{{ t('daemon.notificationsBody') }}</p>
    <p v-else class="voice px-4 pt-3 sm:px-6">{{ t('settings.topicsBody') }}</p>
    <p v-if="error" class="note note-error m-4">{{ error }}</p>
    <LoadingSkeleton v-if="loading" :rows="4" :columns="[3, 4, 1]" class="px-4 py-4 sm:px-6" />
    <div v-else-if="!topics.length" class="px-4 py-8 sm:px-6">
      <p class="voice">{{ t('common.empty') }}</p>
    </div>
    <div v-else class="px-4 py-4 sm:px-6">
      <table class="ledger ledger-stack">
        <thead>
          <tr>
            <th scope="col">{{ t('settings.topic') }}</th>
            <th scope="col">{{ t('settings.descriptionLabel') }}</th>
            <th scope="col">{{ t('settings.preference') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="topic in topics" :key="topic.topic">
            <td :data-label="t('settings.topic')">
              <span class="font-mono text-xs text-base-content">{{ topic.topic }}</span>
            </td>
            <td :data-label="t('settings.descriptionLabel')">
              <span class="text-xs text-dim">{{ topic.description || '—' }}</span>
            </td>
            <td :data-label="t('settings.preference')">
              <select
                class="select select-xs control-select"
                :value="valueFor(topic.topic)"
                :disabled="saving === topic.topic"
                @change="change(topic.topic, $event)"
              >
                <option v-if="isDaemon" value="inherit">{{ t('daemon.none') }}</option>
                <option value="0">{{ t('settings.normal') }}</option>
                <option value="1">{{ t('settings.silent') }}</option>
                <option value="2">{{ t('settings.reject') }}</option>
              </select>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </ConsolePanel>
</template>
