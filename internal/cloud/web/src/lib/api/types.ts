/** Wire types for the MaidKit cloud API. */

export interface Account {
  id: string
  name: string
  nick: string
  is_superuser: boolean
  picture?: string
}

export interface Session {
  account: Account
  expires_in?: number
}

export interface Workspace {
  id: string
  slug: string
  name: string
  type: string
}

export interface Daemon {
  id: string
  workspace_id: string
  name: string
  host_id: string
  enabled: boolean
  terminal_relay_enabled: boolean
  last_seen_at?: string | null
  disconnected_at?: string | null
  created_at: string
  updated_at: string
}

export interface DaemonCredential extends Daemon {
  secret: string
}

export interface Health {
  daemon_id: string
  score: number
  status: string
  evaluated_at: string
  received_at: string
  stale: boolean
}

export interface Metric {
  id: string
  daemon_id: string
  sent_at: string
  received_at: string
  uptime_seconds: number
  process_memory_bytes: number
  cpu_percent: number
  cpu_count: number
  load1: number
  load5: number
  load15: number
  memory_used_percent: number
  memory_used_bytes: number
  memory_total_bytes: number
  swap_total_kb: number
  swap_free_kb: number
  disk_total_kb: number
  disk_available_kb: number
  net_rx_bytes: number
  net_tx_bytes: number
  webhook_executions: number
  webhook_failures: number
  health_score: number
  health_status: string
}

export interface Container {
  daemon_id: string
  container_id: string
  runtime: string
  name: string
  image: string
  state: string
  status: string
  compose_project: string
  first_seen_at: string
  last_seen_at: string
}

export interface LogLine {
  id: string
  daemon_id: string
  container_id: string
  timestamp: string
  received_at: string
  line: string
}

export interface Action {
  name: string
  display_name: string
  enabled: boolean
  notify_on_success: boolean
  notify_on_failure: boolean
  timeout: string
  cwd: string
  user: string
  updated_at: string
}

export interface WebhookResult {
  id: string
  daemon_id: string
  name: string
  body: string
  signature: string
  invoked_by: string
  status: string
  result_code?: number
  result_body?: string
  result_error?: string
  created_at: string
  updated_at: string
}

export interface Notification {
  id: string
  account_id: string
  workspace_id: string
  daemon_id: string
  kind: string
  title: string
  subtitle?: string
  body: string
  metadata?: unknown
  read_at?: string | null
  created_at: string
}

export interface NotificationTopic {
  topic: string
  description: string
}

export interface NotificationPreference {
  id: string
  account_id: string
  workspace_id: string
  daemon_id: string
  topic: string
  preference: number
  created_at: string
  updated_at: string
}

export interface Credential {
  id: string
  label: string
  daemon_ids: string[]
  host_ids: string[]
  action_names: string[]
  created_at: string
  last_used_at?: string | null
}

export interface CredentialToken extends Credential {
  token: string
}

export interface Quota {
  workspace_id: string
  quotas: Record<string, number>
}
