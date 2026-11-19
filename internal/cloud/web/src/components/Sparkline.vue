<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{
  values: number[]
  max?: number
  label: string
}>()

const width = 240
const height = 48
const ceiling = computed(() => {
  const peak = Math.max(...props.values, props.max ?? 0, 1)
  return peak
})

const points = computed(() => {
  if (!props.values.length) return ''
  const step = props.values.length > 1 ? width / (props.values.length - 1) : width
  return props.values
    .map((value, index) => {
      const x = index * step
      const y = height - Math.min(value / ceiling.value, 1) * (height - 4) - 2
      return `${x.toFixed(1)},${y.toFixed(1)}`
    })
    .join(' ')
})
</script>

<template>
  <svg
    :viewBox="`0 0 ${width} ${height}`"
    class="h-12 w-full text-primary"
    role="img"
    :aria-label="label"
    preserveAspectRatio="none"
  >
    <polyline
      v-if="points"
      :points="points"
      fill="none"
      stroke="currentColor"
      stroke-width="1.5"
      stroke-linejoin="round"
      stroke-linecap="round"
      vector-effect="non-scaling-stroke"
    />
  </svg>
</template>
