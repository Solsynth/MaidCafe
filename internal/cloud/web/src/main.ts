import { createApp } from 'vue'
import { createPinia } from 'pinia'

import '@fontsource/nunito/400.css'
import '@fontsource/nunito/600.css'
import '@fontsource/nunito/700.css'
import '@fontsource/ibm-plex-mono/400.css'
import '@fontsource/ibm-plex-mono/500.css'
import './assets/main.css'

import App from './App.vue'
import router from './router'
import { i18n } from './lib/i18n'
import { applyLocale, applyTheme, readLocale, readTheme } from './lib/prefs'

// Apply saved (or system) preferences before the first paint so the shell
// never flashes the other theme or language.
applyTheme(readTheme())
applyLocale(readLocale())

const app = createApp(App)

app.use(createPinia())
app.use(i18n)
app.use(router)

// Mount after the first navigation settles: the session boot strips the
// hand-off code from the address bar, and the router's initial history write
// must not put it back.
await router.isReady()
app.mount('#app')
