<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Renew16 } from '@carbon/icons-vue'

import type { Health, Metric } from '@/lib/api/types'
import { CloudError } from '@/lib/api/client'
import { daemonApi } from '@/lib/api'
import { bytes, duration, percent, stamp } from '@/lib/format'
import ConsolePanel from '@/components/ConsolePanel.vue'
import LoadingSkeleton from '@/components/LoadingSkeleton.vue'
import IconButton from '@/components/IconButton.vue'
import Sparkline from '@/components/Sparkline.vue'
import StatusPill from '@/components/StatusPill.vue'

const props = defineProps<{ daemonId: string }>()
const { t } = useI18n()

const metrics = ref<Metric[]>([])
const health = ref<Health | null>(null)
const loading = ref(true)
const error = ref('')

const latest = computed(() => metrics.value[0] ?? null)
const ordered = computed(() => [...metrics.value].reverse())
const cpuValues = computed(() => ordered.value.map((metric) => metric.cpu_percent))
const memoryValues = computed(() => ordered.value.map((metric) => metric.memory_used_percent))

const healthTone = computed<'success' | 'warning' | 'error' | 'neutral'>(() => {
  if (!health.value) return 'neutral'
  if (health.value.stale) return 'warning'
  switch (health.value.status) {
    case 'ok':
    case 'healthy':
      return 'success'
    case 'warning':
    case 'degraded':
      return 'warning'
    case 'critical':
    case 'error':
      return 'error'
    default:
      return 'neutral'
  }
})

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [samples, current] = await Promise.all([
      daemonApi.metrics(props.daemonId, 60),
      daemonApi.health(props.daemonId).catch((caught) => {
        if (caught instanceof CloudError && caught.isNotFound) return null
        throw caught
      }),
    ])
    metrics.value = samples
    health.value = current
  } catch (caught) {
    error.value = caught instanceof CloudError ? caught.message : t('api.requestFailed')
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="space-y-5">
    <p v-if="error" class="note note-error">{{ error }}</p>
    <LoadingSkeleton v-if="loading" :rows="3" :columns="[2, 2, 2]" />

    <template v-else>
      <ConsolePanel :title="t('daemon.healthTitle')">
        <div class="flex flex-wrap items-center gap-3">
          <StatusPill
            v-if="health"
            :tone="healthTone"
            :label="`${health.status} · ${health.score}`"
          />
          <span v-else class="chip">{{ t('common.unknown') }}</span>
          <span v-if="health" class="text-2xs text-faint">
            {{ t('daemon.evaluated') }} {{ stamp(health.evaluated_at) }}
          </span>
          <span v-if="health?.stale" class="text-2xs text-warning">{{ t('daemon.stale') }}</span>
          <IconButton :label="t('common.refresh')" :disabled="loading" @click="load">
            <Renew16 aria-hidden="true" />
          </IconButton>
        </div>
        <p v-if="!health" class="voice mt-3">{{ t('daemon.neverReported') }}</p>
      </ConsolePanel>

      <template v-if="latest">
        <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <ConsolePanel :title="t('daemon.cpu')">
            <p class="numeric text-lg text-base-content">{{ percent(latest.cpu_percent) }}</p>
            <p class="text-2xs text-faint">{{ latest.cpu_count }} cores</p>
          </ConsolePanel>
          <ConsolePanel :title="t('daemon.memory')">
            <p class="numeric text-lg text-base-content">{{ percent(latest.memory_used_percent) }}</p>
            <p class="text-2xs text-faint">
              {{ bytes(latest.memory_used_bytes) }} / {{ bytes(latest.memory_total_bytes) }}
            </p>
          </ConsolePanel>
          <ConsolePanel :title="t('daemon.disk')">
            <p class="numeric text-lg text-base-content">
              {{ percent(latest.disk_total_kb ? ((latest.disk_total_kb - latest.disk_available_kb) / latest.disk_total_kb) * 100 : 0) }}
            </p>
            <p class="text-2xs text-faint">
              {{ bytes(latest.disk_available_kb * 1024) }} free of {{ bytes(latest.disk_total_kb * 1024) }}
            </p>
          </ConsolePanel>
          <ConsolePanel :title="t('daemon.uptime')">
            <p class="numeric text-lg text-base-content">{{ duration(latest.uptime_seconds) }}</p>
            <p class="text-2xs text-faint">{{ t('daemon.processMemory') }} {{ bytes(latest.process_memory_bytes) }}</p>
          </ConsolePanel>
        </div>

        <div class="grid gap-3 sm:grid-cols-3">
          <ConsolePanel :title="t('daemon.load')">
            <p class="numeric text-base-content">
              {{ latest.load1.toFixed(2) }} / {{ latest.load5.toFixed(2) }} / {{ latest.load15.toFixed(2) }}
            </p>
          </ConsolePanel>
          <ConsolePanel :title="t('daemon.network')">
            <p class="numeric text-base-content">
              ↓ {{ bytes(latest.net_rx_bytes) }} · ↑ {{ bytes(latest.net_tx_bytes) }}
            </p>
          </ConsolePanel>
          <ConsolePanel :title="t('daemon.actionsTitle')">
            <p class="numeric text-base-content">
              {{ latest.webhook_executions }} ran · {{ latest.webhook_failures }} failed
            </p>
          </ConsolePanel>
        </div>

        <ConsolePanel :title="t('daemon.trend')" :note="t('daemon.samples', { count: metrics.length })">
          <div class="grid gap-5 sm:grid-cols-2">
            <div>
              <p class="eyebrow mb-2">{{ t('daemon.cpu') }}</p>
              <Sparkline :values="cpuValues" :max="100" :label="t('daemon.cpu')" />
            </div>
            <div>
              <p class="eyebrow mb-2">{{ t('daemon.memory') }}</p>
              <Sparkline :values="memoryValues" :max="100" :label="t('daemon.memory')" />
            </div>
          </div>
        </ConsolePanel>
      </template>
    </template>
  </div>
</template>
