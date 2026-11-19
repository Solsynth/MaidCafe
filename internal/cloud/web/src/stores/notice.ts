import { defineStore } from 'pinia'
import { ref } from 'vue'

interface Notice {
  id: number
  tone: 'ok' | 'error'
  message: string
}

let nextId = 1

/** The shell's one-line status rail: results print where the work happened. */
export const useNoticeStore = defineStore('notice', () => {
  const items = ref<Notice[]>([])

  function push(tone: Notice['tone'], message: string) {
    const id = nextId++
    items.value = [...items.value.slice(-2), { id, tone, message }]
    window.setTimeout(() => {
      items.value = items.value.filter((item) => item.id !== id)
    }, 6000)
  }

  const ok = (message: string) => push('ok', message)
  const fail = (message: string) => push('error', message)

  return { items, ok, fail, push }
})
