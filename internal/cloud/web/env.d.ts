/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Cloud API base in development. */
  readonly VITE_API_TARGET?: string
  /** Sign-in page override when the server has no [handoff] config. */
  readonly VITE_AUTH_URL?: string
}
