<script setup lang="ts">
import { onMounted } from 'vue'
import { useI18n } from 'vue-i18n'

import { useWorkspaceStore } from '@/stores/workspace'
import SessionGate from '@/components/SessionGate.vue'
import PageHeader from '@/components/PageHeader.vue'
import ConsolePanel from '@/components/ConsolePanel.vue'
import NotificationPreferenceEditor from '@/components/NotificationPreferenceEditor.vue'

const { t } = useI18n()
const workspace = useWorkspaceStore()

onMounted(() => workspace.load())
</script>

<template>
  <SessionGate>
    <PageHeader :title="t('settings.title')" :description="t('settings.description')" />

    <p v-if="workspace.error" class="note note-error mb-4">{{ workspace.error }}</p>

    <div v-if="workspace.selected" class="space-y-5">
      <ConsolePanel :title="t('settings.quotaTitle')" :note="workspace.selected.name">
        <table class="ledger">
          <thead>
            <tr>
              <th scope="col">{{ t('settings.limit') }}</th>
              <th scope="col">{{ t('settings.value') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(value, key) in workspace.quota?.quotas ?? {}" :key="key">
              <td :data-label="t('settings.limit')">
                <span class="font-mono text-xs text-dim">{{ key }}</span>
              </td>
              <td :data-label="t('settings.value')">
                <span class="numeric text-xs text-base-content">{{ value }}</span>
              </td>
            </tr>
          </tbody>
        </table>
      </ConsolePanel>

      <NotificationPreferenceEditor :workspace-id="workspace.selected.id" />
    </div>
  </SessionGate>
</template>
