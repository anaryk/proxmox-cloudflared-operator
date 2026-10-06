import { expect, test } from 'vitest'

import { accountTokenUrl, permissions, tokenName, userTokenUrl } from './tokenTemplate'

const keys =
  '%5B%7B%22key%22%3A%22argotunnel%22%2C%22type%22%3A%22edit%22%7D%2C%7B%22key%22%3A%22dns%22%2C%22type%22%3A%22edit%22%7D%2C%7B%22key%22%3A%22zone%22%2C%22type%22%3A%22read%22%7D%5D'

test('the user token link is the template of Cloudflare with the three permissions and the name of the node', () => {
  expect(userTokenUrl('pve1')).toBe(`https://dash.cloudflare.com/profile/api-tokens?permissionGroupKeys=${keys}&accountId=*&zoneId=all&name=pco%20on%20pve1`)
})

test('the account token link has no account or zone, and picks the account in the dashboard', () => {
  expect(accountTokenUrl('pve1')).toBe(`https://dash.cloudflare.com/?to=/:account/api-tokens&permissionGroupKeys=${keys}&name=pco%20on%20pve1`)
})

test('a node name that needs encoding cannot add a parameter or end the value', () => {
  const node = 'pve 1&x=é/#(a)*'
  const name = 'pco%20on%20pve%201%26x%3D%C3%A9%2F%23%28a%29%2A'
  expect(userTokenUrl(node)).toBe(`https://dash.cloudflare.com/profile/api-tokens?permissionGroupKeys=${keys}&accountId=*&zoneId=all&name=${name}`)
  expect(accountTokenUrl(node)).toBe(`https://dash.cloudflare.com/?to=/:account/api-tokens&permissionGroupKeys=${keys}&name=${name}`)
  const query = new URL(userTokenUrl(node)).searchParams
  expect(query.get('name')).toBe(`pco on ${node}`)
  expect([...query.keys()]).toEqual(['permissionGroupKeys', 'accountId', 'zoneId', 'name'])
})

test('the keys decode to the rows of the token documentation, and only those', () => {
  const decoded = JSON.parse(new URL(userTokenUrl('pve1')).searchParams.get('permissionGroupKeys') ?? '') as unknown
  expect(decoded).toEqual([
    { key: 'argotunnel', type: 'edit' },
    { key: 'dns', type: 'edit' },
    { key: 'zone', type: 'read' },
  ])
  expect(permissions.map((p) => p.row)).toEqual(['Account > Cloudflare Tunnel > Edit', 'Zone > DNS > Edit', 'Zone > Zone > Read'])
  expect(tokenName('pve1')).toBe('pco on pve1')
  expect(tokenName('')).toBe('pco')
})
