<script setup lang="ts">
/**
 * Compact icon-only action for table rows. The label is both the accessible
 * name and the hover tooltip, so the icon never stands alone.
 */
const props = defineProps<{
  label: string
  tone?: 'neutral' | 'danger'
  disabled?: boolean
}>()

const emit = defineEmits<{ click: [] }>()
</script>

<template>
  <button
    type="button"
    class="btn btn-ghost btn-xs px-1"
    :class="props.tone === 'danger' ? 'text-dim hover:text-error' : 'text-dim hover:text-base-content'"
    :title="props.label"
    :aria-label="props.label"
    :disabled="props.disabled"
    @click="emit('click')"
  >
    <slot />
  </button>
</template>
