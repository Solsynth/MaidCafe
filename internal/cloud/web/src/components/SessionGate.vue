<script setup lang="ts">
import { useI18n } from 'vue-i18n'

import { useSessionStore } from '@/stores/session'
import { beginSignIn } from '@/lib/auth'
import LoadingSkeleton from '@/components/LoadingSkeleton.vue'

const session = useSessionStore()
const { t } = useI18n()
</script>

<template>
  <LoadingSkeleton v-if="session.state === 'loading'" :rows="4" variant="rows" />

  <div v-else-if="session.state === 'offline'" class="py-8">
    <h2 class="title text-lg text-base-content">{{ t('app.offline') }}</h2>
    <p class="voice mt-2 max-w-xl">{{ t('app.offlineBody') }}</p>
    <p v-if="session.error" class="note note-error mt-4">{{ session.error }}</p>
    <button type="button" class="btn btn-primary btn-sm mt-4" @click="session.retry()">
      {{ t('common.retry') }}
    </button>
  </div>

  <div v-else-if="!session.signedIn" class="py-8">
    <h2 class="title text-lg text-base-content">{{ t('app.signedOutTitle') }}</h2>
    <p class="voice mt-2 max-w-xl">{{ t('app.signedOutBody') }}</p>
    <button type="button" class="btn btn-primary btn-sm mt-4" @click="beginSignIn">
      {{ t('app.signIn') }}
    </button>
  </div>

  <slot v-else />
</template>
