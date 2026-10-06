// The calls of this page. The settings are written at the revision they were
// read at, a manual route at its own.

import { api } from '../../api/client'
import type { ManualRouteView, Settings, SettingsView } from '../../api/types.gen'

// The answer of PUT /v1/settings: the settings as saved, and those of them
// that take effect only once the daemon is restarted.
export type SavedSettings = SettingsView & { restartNeeded: string[] }

export const getSettings = () => api<SettingsView>('GET', '/api/v1/settings')

export const putSettings = (rev: number, settings: Settings) => api<SavedSettings>('PUT', '/api/v1/settings', { rev, settings })

export const getRoutes = () => api<ManualRouteView[]>('GET', '/api/v1/routes/manual')

const body = (r: ManualRouteView) => ({ id: r.id, hostname: r.hostname, target: r.target, options: r.options })

// A new route has no revision.
export const createRoute = (r: ManualRouteView) => api<ManualRouteView>('POST', '/api/v1/routes/manual', body(r))

export const updateRoute = (r: ManualRouteView, rev: number) =>
  api<ManualRouteView>('PUT', `/api/v1/routes/manual/${encodeURIComponent(r.id)}`, { ...body(r), rev })

export const deleteRoute = (id: string, rev: number) => api<unknown>('DELETE', `/api/v1/routes/manual/${encodeURIComponent(id)}?rev=${rev}`)

export const restartDaemon = () => api<unknown>('POST', '/api/v1/daemon/restart', {})
