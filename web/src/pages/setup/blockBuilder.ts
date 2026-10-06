// The block of routes for the Notes of a guest, written as the parser reads
// it (docs/annotations.md). A block is only built from values that pass the
// parser's own rules, so what is shown and copied is never a line the parser
// would drop.

import { hostnameError, normalHostname, viaError } from '../routes/manual'

// The tag of the fence; the gate tag of the settings is another thing, the
// tag the guest carries in Proxmox VE.
export const fenceTag = 'cf-tunnel'

export interface BlockValues {
  hostname: string
  scheme: 'http' | 'https'
  port: string
  noTLSVerify: boolean
  hostHeader: string
  sni: string
  via: string
}

export const emptyBlock: BlockValues = { hostname: '', scheme: 'http', port: '', noTLSVerify: false, hostHeader: '', sni: '', via: '' }

export type BlockErrors = Partial<Record<keyof BlockValues, string>>

// hostOf puts a name in front of a zone; a name left empty is no hostname, and
// one that ends in the zone already is the hostname.
export function hostOf(label: string, zone: string): string {
  const name = label.trim().replace(/\.+$/, '')
  if (name === '') return ''
  return name.toLowerCase().endsWith(`.${zone.toLowerCase()}`) ? name : `${name}.${zone}`
}

export function blockErrors(v: BlockValues): BlockErrors {
  const e: BlockErrors = {}
  const host = hostnameError(v.hostname.trim())
  if (host) e.hostname = host
  // no leading zeros, as the parser has it
  const port = v.port.trim()
  if (!/^[1-9][0-9]{0,4}$/.test(port) || Number(port) > 65535) e.port = 'want a port from 1 to 65535, without leading zeros'
  const hostHeader = v.hostHeader.trim()
  if (hostHeader && !/^[A-Za-z0-9._:-]{1,253}$/.test(hostHeader)) e.hostHeader = `"${hostHeader}" is not a host name with an optional port`
  const sni = v.sni.trim()
  if (v.scheme === 'https' && sni && hostnameError(sni, false)) e.sni = `"${sni}" is not a host name`
  const via = v.via.trim()
  if (via) {
    const why = viaError(via)
    if (why) e.via = why
  }
  return e
}

// buildBlock is the block for the Notes: one line, the hostname, an arrow, the
// target and the options that apply to it. It is undefined while any value is
// wrong. no-tls-verify and sni belong to https only.
export function buildBlock(v: BlockValues): string | undefined {
  if (Object.values(blockErrors(v)).some(Boolean)) return undefined
  const https = v.scheme === 'https'
  const words = [normalHostname(v.hostname), '->', `${https ? 'https://' : ''}:${v.port.trim()}`]
  if (https && v.noTLSVerify) words.push('no-tls-verify')
  if (v.hostHeader.trim()) words.push(`host-header=${v.hostHeader.trim()}`)
  if (https && v.sni.trim()) words.push(`sni=${v.sni.trim().toLowerCase()}`)
  if (v.via.trim()) words.push(`via=${/^net/i.test(v.via.trim()) ? v.via.trim().toLowerCase() : v.via.trim()}`)
  return `\`\`\`${fenceTag}\n${words.join(' ')}\n\`\`\``
}
