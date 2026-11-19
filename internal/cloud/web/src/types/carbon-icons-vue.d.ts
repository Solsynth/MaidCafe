// @carbon/icons-vue publishes runtime components without TypeScript
// declarations. Only the glyphs this console imports need typing.
declare module '@carbon/icons-vue' {
  import type { DefineComponent } from 'vue'

  export const Add16: DefineComponent
  export const Checkmark16: DefineComponent
  export const ChevronRight16: DefineComponent
  export const Cloud16: DefineComponent
  export const Copy16: DefineComponent
  export const Edit16: DefineComponent
  export const Key16: DefineComponent
  export const Moon16: DefineComponent
  export const Notification16: DefineComponent
  export const Play16: DefineComponent
  export const Renew16: DefineComponent
  export const Settings16: DefineComponent
  export const Sun16: DefineComponent
  export const TrashCan16: DefineComponent
  export const UserAvatar16: DefineComponent
  export const WarningAlt16: DefineComponent
}
