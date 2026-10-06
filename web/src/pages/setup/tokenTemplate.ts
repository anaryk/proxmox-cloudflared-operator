// The links that open Cloudflare's form for a new API token with the three
// permissions of docs/cloudflare-token.md already chosen. They are built from
// the constants below and the name of the node, never from anything the
// daemon, a guest or Cloudflare sent.

export interface Permission {
  // the row as the dashboard's form shows it, and what pco does with it
  row: string
  why: string
  // the key and type of Cloudflare's permissionGroupKeys, for a row that
  // Cloudflare documents a key for
  link?: { key: string; type: 'read' | 'edit' }
}

// Keys from https://developers.cloudflare.com/fundamentals/api/how-to/account-owned-token-template/
// as read on 2026-10-06. The page lists dns and zone and no key for
// Cloudflare Tunnel, so that row is left to the admin: a key that is guessed
// is dropped silently by the form when it is wrong.
export const permissions: readonly Permission[] = [
  {
    row: 'Account > Cloudflare Tunnel > Edit',
    why: 'to create the tunnel, write its configuration and read the token a connector runs with',
  },
  {
    row: 'Zone > DNS > Edit',
    why: 'to create, change and delete the CNAME record of each hostname',
    link: { key: 'dns', type: 'edit' },
  },
  {
    row: 'Zone > Zone > Read',
    why: 'to list the zones',
    link: { key: 'zone', type: 'read' },
  },
]

// encode is encodeURIComponent and the five characters it leaves as they are.
const encode = (s: string) => encodeURIComponent(s).replace(/[!'()*]/g, (c) => `%${c.charCodeAt(0).toString(16).toUpperCase()}`)

// A session that names no node leaves the name at the product's.
export const tokenName = (node: string) => (node ? `pco on ${node}` : 'pco')

const keys = () => encode(JSON.stringify(permissions.flatMap((p) => (p.link ? [p.link] : []))))

// userTokenUrl opens the form for a token that belongs to the person signed
// in to the dashboard, over all accounts and zones to be narrowed there.
export function userTokenUrl(node: string): string {
  return `https://dash.cloudflare.com/profile/api-tokens?permissionGroupKeys=${keys()}&accountId=*&zoneId=all&name=${encode(tokenName(node))}`
}

// accountTokenUrl opens the form for a token that belongs to an account,
// which the dashboard asks the person to pick.
export function accountTokenUrl(node: string): string {
  return `https://dash.cloudflare.com/?to=/:account/api-tokens&permissionGroupKeys=${keys()}&name=${encode(tokenName(node))}`
}
