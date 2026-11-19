<script setup lang="ts">
import { useI18n } from 'vue-i18n'

const { t } = useI18n()
</script>

<template>
  <div class="py-12">
    <h1 class="title text-lg text-base-content">{{ t('common.empty') }}</h1>
    <p class="voice mt-2">{{ t('api.notFound') }}</p>
    <RouterLink to="/" class="btn btn-primary btn-sm mt-4">{{ t('app.fleet') }}</RouterLink>
  </div>
</template>
