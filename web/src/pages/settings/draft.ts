// What the form edits: the settings as text, lists a line each, numbers as
// typed, so that half a number does not turn into another one on the way.

import type { Settings } from '../../api/types.gen'
import { type FieldIssue, normalizeHostname, normalizePattern, settingFields } from './validate'

export interface Draft {
  gateTag: string
  allowHosts: string
  denyHosts: string
  pollInterval: string
  grace: string
  trustStatic: boolean
  trustedCIDRs: string
  manualCIDRs: string
  admission: string
  zonePins: string
  observeOnly: boolean
  identityMinimum: string
  maxHostnamesPerGuest: string
  reverifyInterval: string
  cloudflareBudget: string
}

export function draftOf(s: Settings): Draft {
  return {
    gateTag: s.gateTag,
    allowHosts: (s.allowHosts ?? []).join('\n'),
    denyHosts: (s.denyHosts ?? []).join('\n'),
    pollInterval: s.pollInterval,
    grace: s.grace,
    trustStatic: s.trustStatic ?? false,
    trustedCIDRs: (s.trustedCIDRs ?? []).join('\n'),
    manualCIDRs: (s.manualCIDRs ?? []).join('\n'),
    admission: s.admission,
    zonePins: Object.entries(s.zonePins ?? {})
      .map(([zone, id]) => `${zone} ${id}`)
      .join('\n'),
    observeOnly: s.observeOnly,
    identityMinimum: s.identityMinimum,
    maxHostnamesPerGuest: String(s.maxHostnamesPerGuest),
    reverifyInterval: s.reverifyInterval,
    cloudflareBudget: String(s.cloudflareBudget),
  }
}

// entries are the items of a list: one a line, and a comma or a space
// between items as well, none of which a pattern or a prefix may hold.
const entries = (text: string) => text.split(/[\s,]+/).filter(Boolean)

const count = (text: string) => (/^[+-]?\d+$/.test(text.trim()) ? Number(text.trim()) : Number.NaN)

// pins reads "zone credential" lines; a zone alone is a pin without its
// credential id, which the validation refuses.
function pins(text: string): [string, string][] {
  const out: [string, string][] = []
  for (const line of text.split(/\r?\n/)) {
    const [, zone = '', id = ''] = /^\s*([^\s=]+)\s*[\s=]?\s*(.*?)\s*$/.exec(line) ?? []
    if (zone !== '') out.push([zone, id])
  }
  return out
}

// The settings the daemon leaves out when they are empty: the form sets them again.
const optional = new Set<string>(settingFields.filter((f) => f.optional).map((f) => f.name))

const normalized = (list: string[], normalize: (s: string) => { value?: string }) => list.map((p) => normalize(p).value ?? p)

// settingsOf makes the settings of a draft. A pattern that is valid is
// written in its normal form, one that is not stays as typed for the
// validation to name. base is what the settings were read as: a field the
// page does not know is sent back as it was.
export function settingsOf(d: Draft, base?: Settings): Settings {
  const allowHosts = normalized(entries(d.allowHosts), normalizePattern)
  const denyHosts = normalized(entries(d.denyHosts), normalizePattern)
  const trustedCIDRs = entries(d.trustedCIDRs)
  const manualCIDRs = entries(d.manualCIDRs)
  const zonePins = Object.fromEntries(pins(d.zonePins).map(([zone, id]) => [normalizeHostname(zone).value ?? zone, id]))
  return {
    ...Object.fromEntries(Object.entries(base ?? {}).filter(([name]) => !optional.has(name))),
    gateTag: d.gateTag,
    pollInterval: d.pollInterval.trim(),
    grace: d.grace.trim(),
    admission: d.admission,
    observeOnly: d.observeOnly,
    identityMinimum: d.identityMinimum,
    maxHostnamesPerGuest: count(d.maxHostnamesPerGuest),
    reverifyInterval: d.reverifyInterval.trim(),
    cloudflareBudget: count(d.cloudflareBudget),
    ...(allowHosts.length > 0 && { allowHosts }),
    ...(denyHosts.length > 0 && { denyHosts }),
    ...(d.trustStatic && { trustStatic: true }),
    ...(trustedCIDRs.length > 0 && { trustedCIDRs }),
    ...(manualCIDRs.length > 0 && { manualCIDRs }),
    ...(Object.keys(zonePins).length > 0 && { zonePins }),
  }
}

// draftIssues are what only the text can show: a zone on two lines, or two
// that differ in case only, would be one pin in the settings.
export function draftIssues(d: Draft): FieldIssue[] {
  const seen = new Set<string>()
  const out: FieldIssue[] = []
  for (const [zone] of pins(d.zonePins)) {
    const name = normalizeHostname(zone).value ?? zone
    if (!seen.has(name)) {
      seen.add(name)
      continue
    }
    const field = `zonePins[${JSON.stringify(zone)}]`
    if (!out.some((i) => i.field === field)) out.push({ field, message: `${field}: named on two lines` })
  }
  return out
}

// addAllowHost adds a pattern to the allow list, once.
export function addAllowHost(d: Draft, pattern: string): Draft {
  const have = entries(d.allowHosts)
  if (have.some((p) => p.toLowerCase() === pattern.toLowerCase())) return d
  return { ...d, allowHosts: [...have, pattern].join('\n') }
}

// mergeDraft puts the edits of a draft on settings that changed since it was
// made: a field the user edited keeps their text, every other field is as it
// is now. was and now are the drafts of the settings before and after.
export function mergeDraft(mine: Draft, was: Draft, now: Draft): Draft {
  const keys = Object.keys(mine) as (keyof Draft)[]
  return Object.fromEntries(keys.map((k) => [k, mine[k] === was[k] ? now[k] : mine[k]])) as unknown as Draft
}
