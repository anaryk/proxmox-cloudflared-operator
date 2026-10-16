import { api } from '../../api/client'
import type { SegmentView } from '../../api/types.gen'

export const segmentKey = (s: SegmentView) => `${s.bridge}:${s.vlan ?? ''}`

export const segmentName = (s: SegmentView) => (s.vlan === undefined ? s.bridge : `${s.bridge} VLAN ${s.vlan}`)

// waitingSegments are the segments nobody acknowledged that matter: one a
// route at observed was proven on, or one an appliance's card is on, where
// the guests it reaches are.
export const waitingSegments = (segments: readonly SegmentView[]) => segments.filter((s) => !s.acknowledged && (s.own || s.routes > 0))

export async function acknowledgeSegment(s: SegmentView) {
  await api('POST', '/api/v1/segments/acknowledge', { bridge: s.bridge, vlan: s.vlan ?? 0 })
}

export async function revokeSegment(s: SegmentView) {
  await api('POST', '/api/v1/segments/revoke', { bridge: s.bridge, vlan: s.vlan ?? 0 })
}
