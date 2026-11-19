<script setup lang="ts">
defineProps<{
  title: string
  description?: string
}>()
</script>

<template>
  <header class="mb-7 flex flex-wrap items-end justify-between gap-4">
    <div>
      <h1 class="title mt-2 text-xl text-base-content sm:text-2xl">{{ title }}</h1>
      <p v-if="description" class="voice mt-2 max-w-2xl">{{ description }}</p>
    </div>
    <div class="flex flex-wrap items-center gap-2">
      <slot name="actions" />
    </div>
  </header>
</template>
