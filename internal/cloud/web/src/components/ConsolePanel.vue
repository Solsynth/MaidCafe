<script setup lang="ts">
withDefaults(
  defineProps<{
    title?: string
    note?: string
    flush?: boolean
  }>(),
  { title: '', note: undefined, flush: false },
)
</script>

<template>
  <section class="overflow-hidden rounded-card bg-base-200/50">
    <header
      v-if="title || note || $slots.header"
      class="flex flex-wrap items-baseline justify-between gap-x-2 gap-y-1 px-4 pb-1.5 pt-4 sm:px-6"
    >
      <div class="flex min-w-0 flex-wrap items-baseline gap-x-2">
        <h2 v-if="title" class="text-2xs uppercase tracking-[0.14em] text-dim">{{ title }}</h2>
        <span v-if="note" class="text-2xs text-faint">{{ note }}</span>
      </div>
      <div class="ml-auto flex w-full flex-wrap items-center justify-end gap-1 sm:w-auto sm:gap-2">
        <slot name="header" />
      </div>
    </header>
    <div :class="flush ? '' : 'px-4 py-4 sm:px-6'">
      <slot />
    </div>
    <footer v-if="$slots.footer" class="bg-base-200/40 px-4 py-3 sm:px-6">
      <slot name="footer" />
    </footer>
  </section>
</template>
