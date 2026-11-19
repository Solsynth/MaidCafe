import { request } from './client'
import type {
  Action,
  Container,
  Credential,
  CredentialToken,
  Daemon,
  DaemonCredential,
  Health,
  LogLine,
  Metric,
  Notification,
  NotificationPreference,
  NotificationTopic,
  Quota,
  Session,
  WebhookResult,
  Workspace,
} from './types'

export interface AuthConfig {
  configured: boolean
  sign_in_url: string
}

export const sessionApi = {
  config: () => request<AuthConfig>('/auth/config'),
  me: () => request<Session>('/auth/session'),
  redeem: (code: string) =>
    request<Session>('/auth/session', {
      method: 'POST',
      body: { code },
      retryOnUnauthorized: false,
    }),
  refresh: () =>
    request<Session>('/auth/refresh', { method: 'POST', retryOnUnauthorized: false }),
  logout: () => request<void>('/auth/logout', { method: 'POST', retryOnUnauthorized: false }),
}

export const workspaceApi = {
  list: () => request<Workspace[]>('/workspaces'),
  quota: (workspaceID: string) => request<Quota>(`/workspaces/${encodeURIComponent(workspaceID)}/quota`),
}

export const daemonApi = {
  list: (workspaceID: string) =>
    request<Daemon[]>('/daemons', { query: { workspace_id: workspaceID } }),
  create: (workspaceID: string, name: string) =>
    request<DaemonCredential>('/daemons', { method: 'POST', body: { workspace_id: workspaceID, name } }),
  get: (id: string) => request<Daemon>(`/daemons/${encodeURIComponent(id)}`),
  update: (
    id: string,
    patch: { name?: string; enabled?: boolean; terminal_relay_enabled?: boolean },
  ) => request<Daemon>(`/daemons/${encodeURIComponent(id)}`, { method: 'PATCH', body: patch }),
  rotateSecret: (id: string) =>
    request<{ secret: string }>(`/daemons/${encodeURIComponent(id)}/rotate-secret`, { method: 'POST' }),
  disable: (id: string) =>
    request<void>(`/daemons/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  health: (id: string) => request<Health>(`/daemons/${encodeURIComponent(id)}/health`),
  metrics: (id: string, limit = 60) =>
    request<Metric[]>(`/daemons/${encodeURIComponent(id)}/metrics`, { query: { limit } }),
  containers: (id: string, query: { compose?: string; state?: string } = {}) =>
    request<{ containers: Container[] }>(`/daemons/${encodeURIComponent(id)}/containers`, { query }),
  logs: (id: string, query: { container_id?: string; limit?: number; before?: string } = {}) =>
    request<{ logs: LogLine[] }>(`/daemons/${encodeURIComponent(id)}/logs`, { query }),
  actions: (id: string) => request<Action[]>(`/daemons/${encodeURIComponent(id)}/actions`),
  run: (id: string, name: string, body = '') =>
    request<{ id: string; status: string; created_at: string }>(
      `/daemons/${encodeURIComponent(id)}/webhook-requests`,
      { method: 'POST', body: { name, body: btoa(body), signature: '' } },
    ),
  runResult: (id: string, requestID: string) =>
    request<WebhookResult>(
      `/daemons/${encodeURIComponent(id)}/webhook-requests/${encodeURIComponent(requestID)}`,
    ),
}

export const notificationApi = {
  list: (workspaceID: string, query: { unread?: boolean; daemon_id?: string; limit?: number } = {}) =>
    request<Notification[]>('/notifications', { query: { workspace_id: workspaceID, ...query } }),
  markRead: (id: string) =>
    request<void>(`/notifications/${encodeURIComponent(id)}/read`, { method: 'POST' }),
  markAllRead: (workspaceID: string) =>
    request<void>('/notifications/all/read', { method: 'POST', query: { workspace_id: workspaceID } }),
  topics: (workspaceID: string) =>
    request<NotificationTopic[]>('/notification-topics', { query: { workspace_id: workspaceID } }),
  preferences: (workspaceID: string, daemonID = '') =>
    request<NotificationPreference[]>('/notification-preferences', {
      query: { workspace_id: workspaceID, daemon_id: daemonID },
    }),
  setPreference: (topic: string, preference: number, workspaceID: string) =>
    request<void>(`/notification-preferences/${encodeURIComponent(topic)}`, {
      method: 'PUT',
      body: { preference },
      query: { workspace_id: workspaceID },
    }),
  daemonPreferences: (daemonID: string, workspaceID: string) =>
    request<NotificationPreference[]>(
      `/daemons/${encodeURIComponent(daemonID)}/notification-preferences`,
      { query: { workspace_id: workspaceID } },
    ),
  setDaemonPreference: (daemonID: string, topic: string, preference: number, workspaceID: string) =>
    request<void>(`/daemons/${encodeURIComponent(daemonID)}/notification-preferences/${encodeURIComponent(topic)}`, {
      method: 'PUT',
      body: { workspace_id: workspaceID, preference },
    }),
  removeDaemonPreference: (daemonID: string, topic: string, workspaceID: string) =>
    request<void>(`/daemons/${encodeURIComponent(daemonID)}/notification-preferences/${encodeURIComponent(topic)}`, {
      method: 'DELETE',
      query: { workspace_id: workspaceID },
    }),
}

export const credentialApi = {
  list: () => request<Credential[]>('/credentials'),
  create: (body: {
    label: string
    daemon_ids: string[]
    host_ids: string[]
    action_names: string[]
  }) => request<CredentialToken>('/credentials', { method: 'POST', body }),
  remove: (id: string) => request<void>(`/credentials/${encodeURIComponent(id)}`, { method: 'DELETE' }),
}
