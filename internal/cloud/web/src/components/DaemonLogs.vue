<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Renew16 } from '@carbon/icons-vue'

import type { Container, LogLine } from '@/lib/api/types'
import { CloudError } from '@/lib/api/client'
import { daemonApi } from '@/lib/api'
import { clock, stamp } from '@/lib/format'
import ConsolePanel from '@/components/ConsolePanel.vue'
import LoadingSkeleton from '@/components/LoadingSkeleton.vue'
import IconButton from '@/components/IconButton.vue'

const props = defineProps<{ daemonId: string }>()
const { t } = useI18n()

const containers = ref<Container[]>([])
const logs = ref<LogLine[]>([])
const containerID = ref('')
const limit = ref(200)
const loading = ref(true)
const error = ref('')

const chronological = computed(() => [...logs.value].reverse())
const options = computed(() =>
  containers.value.map((container) => ({
    id: container.container_id,
    label: container.name || container.container_id,
  })),
)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const response = await daemonApi.logs(props.daemonId, {
      container_id: containerID.value || undefined,
      limit: limit.value,
    })
    logs.value = response.logs
  } catch (caught) {
    error.value = caught instanceof CloudError ? caught.message : t('api.requestFailed')
  } finally {
    loading.value = false
  }
}

async function loadContainers() {
  try {
    containers.value = (await daemonApi.containers(props.daemonId)).containers
  } catch {
    containers.value = []
  }
}

onMounted(async () => {
  await Promise.all([loadContainers(), load()])
})

watch([containerID, limit], () => void load())
</script>

<template>
  <ConsolePanel :title="t('daemon.logsTitle')" :note="String(logs.length)" flush>
    <template #header>
      <select v-model="containerID" class="select select-xs control-select" :aria-label="t('daemon.container')">
        <option value="">{{ t('daemon.allContainers') }}</option>
        <option v-for="option in options" :key="option.id" :value="option.id">{{ option.label }}</option>
      </select>
      <select v-model.number="limit" class="select select-xs control-select" :aria-label="t('daemon.lines')">
        <option :value="100">100</option>
        <option :value="200">200</option>
        <option :value="500">500</option>
      </select>
      <IconButton :label="t('common.refresh')" :disabled="loading" @click="load">
        <Renew16 aria-hidden="true" />
      </IconButton>
    </template>

    <p v-if="error" class="note note-error m-4">{{ error }}</p>
    <LoadingSkeleton v-if="loading" :rows="6" :columns="[1, 4]" variant="columns" class="px-4 py-4 sm:px-6" />
    <div v-else-if="!logs.length" class="px-4 py-8 sm:px-6">
      <p class="voice">{{ t('daemon.noLogs') }}</p>
      <p class="field-hint">{{ t('daemon.logsHint') }}</p>
    </div>
    <div v-else class="max-h-[32rem] overflow-auto border-t border-line-soft px-4 py-3 font-mono text-2xs leading-relaxed sm:px-6">
      <div v-for="line in chronological" :key="line.id" class="flex gap-3 py-0.5">
        <span class="shrink-0 whitespace-nowrap text-faint" :title="stamp(line.timestamp)">{{ clock(line.timestamp) }}</span>
        <span class="w-24 shrink-0 truncate text-dim" :title="line.container_id">{{ line.container_id.slice(0, 12) }}</span>
        <span class="min-w-0 whitespace-pre-wrap break-all text-base-content">{{ line.line }}</span>
      </div>
    </div>
  </ConsolePanel>
</template>
