<script setup lang="ts">
import { computed } from 'vue'
import { SkeletonBox } from 'skullet/vue'

const props = withDefaults(
  defineProps<{
    rows?: number
    columns?: number[]
    height?: string
    variant?: 'columns' | 'rows'
    compact?: boolean
  }>(),
  {
    rows: 4,
    columns: () => [3, 2],
    height: '0.75rem',
    variant: 'columns',
    compact: false,
  },
)

const total = computed(() => props.columns.reduce((sum, width) => sum + width, 0))
const widthOf = (width: number) => `${(width / total.value) * 100}%`
</script>

<template>
  <div :class="compact ? 'flex min-h-32 items-center' : ''">
    <div class="w-full">
      <div v-if="variant === 'rows'" class="flex flex-col gap-1.5">
        <div v-for="row in rows" :key="`row-${row}`" class="space-y-2 rounded-chip bg-raised px-3 py-3">
          <div class="flex items-center gap-2">
            <SkeletonBox height="0.75rem" width="8rem" aria-hidden="true" />
            <SkeletonBox height="1rem" width="3.5rem" aria-hidden="true" />
          </div>
          <SkeletonBox height="0.5rem" width="100%" class="max-w-md" aria-hidden="true" />
          <SkeletonBox height="0.5rem" width="6rem" aria-hidden="true" />
        </div>
      </div>

      <div v-else class="flex flex-col gap-1.5">
        <div v-for="row in rows" :key="row" class="flex items-center gap-4 rounded-chip bg-raised px-3 py-3">
          <SkeletonBox
            v-for="(width, index) in columns"
            :key="`${row}-${index}`"
            :height="height"
            :width="widthOf(width)"
            aria-hidden="true"
          />
        </div>
      </div>
    </div>
  </div>
</template>
