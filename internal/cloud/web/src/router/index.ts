import { createRouter, createWebHistory } from 'vue-router'
import type { RouteMeta } from 'vue-router'

import { i18n } from '@/lib/i18n'
import { useSessionStore } from '@/stores/session'

declare module 'vue-router' {
  interface RouteMeta {
    titleKey?: string
    narrow?: boolean
    flush?: boolean
  }
}

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    {
      path: '/',
      name: 'fleet',
      component: () => import('@/views/FleetView.vue'),
      meta: { titleKey: 'fleet.title' },
    },
    {
      path: '/daemons/:id',
      name: 'daemon',
      component: () => import('@/views/DaemonView.vue'),
      meta: { titleKey: 'daemon.title' },
    },
    {
      path: '/notifications',
      name: 'notifications',
      component: () => import('@/views/NotificationsView.vue'),
      meta: { titleKey: 'notifications.title' },
    },
    {
      path: '/access',
      name: 'access',
      component: () => import('@/views/AccessView.vue'),
      meta: { titleKey: 'access.title' },
    },
    {
      path: '/settings',
      name: 'settings',
      component: () => import('@/views/SettingsView.vue'),
      meta: { titleKey: 'settings.title' },
    },
    {
      path: '/:pathMatch(.*)*',
      name: 'not-found',
      component: () => import('@/views/NotFoundView.vue'),
      meta: { titleKey: 'app.fleet', narrow: true },
    },
  ],
  scrollBehavior: () => ({ top: 0 }),
})

export function applyRouteTitle(meta: RouteMeta) {
  const key = meta.titleKey
  const title = key ? i18n.global.t(key) : ''
  document.title = title ? `${title} · MaidKit Cloud` : 'MaidKit Cloud'
}

router.afterEach((to) => {
  applyRouteTitle(to.meta)
  const session = useSessionStore()
  if (session.state === 'loading') void session.boot()
})

export default router
