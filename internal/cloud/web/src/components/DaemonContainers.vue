<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Renew16 } from '@carbon/icons-vue'

import type { Container } from '@/lib/api/types'
import { CloudError } from '@/lib/api/client'
import { daemonApi } from '@/lib/api'
import { relative, stamp } from '@/lib/format'
import ConsolePanel from '@/components/ConsolePanel.vue'
import LoadingSkeleton from '@/components/LoadingSkeleton.vue'
import IconButton from '@/components/IconButton.vue'

const props = defineProps<{ daemonId: string }>()
const { t } = useI18n()

const containers = ref<Container[]>([])
const loading = ref(true)
const error = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    containers.value = (await daemonApi.containers(props.daemonId)).containers
  } catch (caught) {
    error.value = caught instanceof CloudError ? caught.message : t('api.requestFailed')
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <ConsolePanel :title="t('daemon.containersTitle')" :note="String(containers.length)" flush>
    <template #header>
      <IconButton :label="t('common.refresh')" :disabled="loading" @click="load">
        <Renew16 aria-hidden="true" />
      </IconButton>
    </template>
    <p v-if="error" class="note note-error m-4">{{ error }}</p>
    <LoadingSkeleton v-if="loading" :rows="4" :columns="[3, 3, 1, 1]" class="px-4 py-4 sm:px-6" />
    <div v-else-if="!containers.length" class="px-4 py-8 sm:px-6">
      <p class="voice">{{ t('daemon.containersEmpty') }}</p>
    </div>
    <div v-else class="px-4 py-4 sm:px-6">
      <table class="ledger ledger-cards">
        <thead>
          <tr>
            <th scope="col">{{ t('daemon.containerName') }}</th>
            <th scope="col">{{ t('daemon.image') }}</th>
            <th scope="col">{{ t('daemon.state') }}</th>
            <th scope="col">{{ t('daemon.runtime') }}</th>
            <th scope="col">{{ t('daemon.compose') }}</th>
            <th scope="col">{{ t('fleet.lastSeen') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="container in containers" :key="`${container.runtime}-${container.container_id}`">
            <td :data-label="t('daemon.containerName')">
              <div class="cell-stack">
                <span class="font-semibold text-base-content">{{ container.name || container.container_id }}</span>
                <span class="font-mono text-2xs text-faint" :title="container.container_id">
                  {{ container.container_id.slice(0, 12) }}
                </span>
              </div>
            </td>
            <td :data-label="t('daemon.image')">
              <span class="break-all font-mono text-2xs text-dim">{{ container.image }}</span>
            </td>
            <td :data-label="t('daemon.state')">{{ container.state || '—' }}</td>
            <td :data-label="t('daemon.runtime')">{{ container.runtime || '—' }}</td>
            <td :data-label="t('daemon.compose')">{{ container.compose_project || '—' }}</td>
            <td :data-label="t('fleet.lastSeen')" :title="stamp(container.last_seen_at)">
              {{ relative(container.last_seen_at) }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </ConsolePanel>
</template>
