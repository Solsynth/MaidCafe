<script setup lang="ts">
withDefaults(
  defineProps<{
    tone: 'success' | 'warning' | 'error' | 'info' | 'neutral'
    label: string
  }>(),
  { tone: 'neutral' },
)

const toneClass: Record<string, string> = {
  success: 'bg-success/12 text-success',
  warning: 'bg-warning/12 text-warning',
  error: 'bg-error/12 text-error',
  info: 'bg-info/12 text-info',
  neutral: 'bg-base-300/60 text-dim',
}
</script>

<template>
  <span class="inline-flex items-center gap-1.5 rounded-chip px-2 py-0.5 text-2xs" :class="toneClass[tone]">
    <span class="h-1.5 w-1.5 rounded-full bg-current" aria-hidden="true" />
    {{ label }}
  </span>
</template>
