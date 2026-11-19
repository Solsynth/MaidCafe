<script setup lang="ts">
import { onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Cloud16,
  Key16,
  Moon16,
  Notification16,
  Settings16,
  Sun16,
  UserAvatar16,
} from '@carbon/icons-vue'
import { RouterLink, RouterView, useRoute } from 'vue-router'

import { useSessionStore } from '@/stores/session'
import { useWorkspaceStore } from '@/stores/workspace'
import { useUiStore } from '@/stores/ui'
import { beginSignIn } from '@/lib/auth'

const session = useSessionStore()
const workspace = useWorkspaceStore()
const ui = useUiStore()
const route = useRoute()
const { t } = useI18n()

const nav = [
  { to: '/', labelKey: 'app.fleet', icon: Cloud16 },
  { to: '/notifications', labelKey: 'app.inbox', icon: Notification16 },
  { to: '/access', labelKey: 'app.access', icon: Key16 },
  { to: '/settings', labelKey: 'app.settings', icon: Settings16 },
]

const year = new Date().getFullYear()

onMounted(() => {
  void session.boot().then(() => workspace.load())
})

function changeLocale(event: Event) {
  ui.setLocale((event.target as HTMLSelectElement).value as 'en' | 'zh-CN')
}

function changeWorkspace(event: Event) {
  workspace.select((event.target as HTMLSelectElement).value)
  void workspace.load(true)
}
</script>

<template>
  <div class="flex min-h-dvh flex-col bg-base-100">
    <a
      href="#main"
      class="sr-only focus:not-sr-only focus:absolute focus:left-4 focus:top-4 focus:z-50 focus:bg-base-300 focus:px-3 focus:py-2 focus:text-xs"
    >
      {{ t('app.skipToContent') }}
    </a>

    <header class="sticky top-0 z-30 bg-base-100/95 backdrop-blur">
      <div class="flex h-12 items-center gap-3 px-3 sm:px-5">
        <RouterLink to="/" class="flex shrink-0 items-center gap-2.5" :aria-label="t('app.fleet')">
          <span class="brand-mark h-[20px] w-[20px] shrink-0" aria-hidden="true" />
          <span class="text-[13px] font-semibold tracking-[0.14em] text-base-content">
            {{ t('app.brand') }}
          </span>
        </RouterLink>

        <span class="hidden text-2xs tracking-[0.04em] text-faint sm:inline">
          {{ t('app.network') }}
        </span>

        <nav class="ml-2 hidden items-center gap-1 md:flex" :aria-label="t('app.fleet')">
          <RouterLink
            v-for="item in nav"
            :key="item.to"
            :to="item.to"
            class="nav-tab"
            :class="route.path === item.to || (item.to === '/' && route.path.startsWith('/daemons')) ? 'nav-tab-active' : ''"
          >
            <component :is="item.icon" class="shrink-0" aria-hidden="true" />
            {{ t(item.labelKey) }}
          </RouterLink>
        </nav>

        <div class="ml-auto flex items-center gap-1.5">
          <label v-if="session.signedIn && workspace.list.length" class="hidden items-center gap-1.5 sm:flex">
            <span class="text-2xs uppercase tracking-[0.12em] text-faint">{{ t('app.workspace') }}</span>
            <select
              class="select select-xs control-select"
              :value="workspace.selectedID"
              :aria-label="t('app.chooseWorkspace')"
              @change="changeWorkspace"
            >
              <option v-for="item in workspace.list" :key="item.id" :value="item.id">
                {{ item.name }}
              </option>
            </select>
          </label>

          <button v-if="!session.signedIn" type="button" class="btn btn-primary btn-xs" @click="beginSignIn">
            {{ t('app.signIn') }}
          </button>

          <div v-else class="dropdown dropdown-end">
            <div tabindex="0" role="button" class="flex items-center gap-2" :title="session.displayName">
              <img
                v-if="session.account?.picture"
                :src="session.account.picture"
                alt=""
                class="h-6 w-6 rounded-full object-cover"
              />
              <span v-else class="flex h-6 w-6 items-center justify-center rounded-full bg-base-300/60 text-faint">
                <UserAvatar16 aria-hidden="true" />
              </span>
              <span class="hidden max-w-32 truncate text-xs text-dim sm:inline">{{ session.displayName }}</span>
            </div>
            <ul tabindex="0" class="menu-plate dropdown-content z-50 mt-2 w-44 p-1.5">
              <template v-for="item in nav" :key="item.to">
                <li class="md:hidden">
                  <RouterLink :to="item.to" class="post-menu-row">{{ t(item.labelKey) }}</RouterLink>
                </li>
              </template>
              <li>
                <button type="button" class="post-menu-row" @click="session.signOut()">
                  {{ t('app.signOut') }}
                </button>
              </li>
            </ul>
          </div>
        </div>
      </div>
    </header>

    <div
      v-if="session.signedIn && workspace.list.length"
      class="flex items-center gap-2 border-t border-line-soft px-4 py-2 sm:hidden"
    >
      <span class="text-2xs uppercase tracking-[0.12em] text-faint">{{ t('app.workspace') }}</span>
      <select
        class="select select-xs control-select"
        :value="workspace.selectedID"
        :aria-label="t('app.chooseWorkspace')"
        @change="changeWorkspace"
      >
        <option v-for="item in workspace.list" :key="item.id" :value="item.id">{{ item.name }}</option>
      </select>
    </div>

    <main id="main" class="min-w-0 flex-1">
      <div class="mx-auto w-full max-w-[1440px] px-4 py-6 sm:px-7 sm:py-8">
        <RouterView />
      </div>
    </main>
    <footer class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 bg-base-200/30 px-4 py-3 text-2xs text-faint sm:px-7">
      <span>{{ t('app.copyright', { year }) }}</span>
      <div class="flex flex-wrap items-center gap-2">
        <span class="hidden sm:inline">{{ t('app.footer') }}</span>
        <button
          type="button"
          class="btn btn-ghost btn-xs px-1.5"
          :title="ui.isDark ? t('app.switchToLight') : t('app.switchToDark')"
          :aria-label="ui.isDark ? t('app.switchToLight') : t('app.switchToDark')"
          @click="ui.toggleTheme()"
        >
          <component :is="ui.isDark ? Sun16 : Moon16" aria-hidden="true" />
        </button>
        <select
          class="select select-xs footer-locale"
          :value="ui.locale"
          :aria-label="t('app.language')"
          @change="changeLocale"
        >
          <option value="en">{{ t('app.languageEnglish') }}</option>
          <option value="zh-CN">{{ t('app.languageChinese') }}</option>
        </select>
      </div>
    </footer>
  </div>
</template>
