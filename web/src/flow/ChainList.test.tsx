import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, test } from 'vitest'

import type { State, TrafficView } from '../api/types.gen'
import { routeKey } from '../text/routes'
import { ChainList } from './ChainList'
import chains from './testdata/chains.json'
import { scaled } from './testdata/scale'
import traffic from './testdata/traffic.json'

const st = chains as unknown as State
const tv = traffic as unknown as TrafficView

const rows = () => screen.getAllByRole('listitem').filter((li) => li.classList.contains('chain'))
const head = (hostname: string, owner: string) => {
  const li = rows().find((x) => x.querySelector('.chain-host')?.textContent === hostname && x.querySelector('.chain-owner')?.textContent?.endsWith(owner))
  const button = li?.querySelector('button')
  if (!button) throw new Error(`no row ${hostname} ${owner}`)
  return button
}

describe('the chain list', () => {
  test('a row per hostname, with its state in a word', () => {
    render(<ChainList state={st} traffic={tv} />)
    expect(
      rows().map((li) => [li.querySelector('.chain-host')?.textContent, li.querySelector('.status-word')?.textContent, [...li.querySelectorAll('.badge')].map((b) => b.textContent).join(' ')]),
    ).toEqual([
      ['api.example.com', 'unreachable', 'DNS'],
      ['app.example.com', 'active', ''],
      ['dns.example.com', 'waits for approval', ''],
      ['example.com', 'rejected', ''],
      ['held.example.com', 'held', '503'],
      ['intranet.example.com', 'active', ''],
      ['lab.example.dev', 'active', ''],
      ['new.example.com', 'waits for approval', ''],
      ['old.example.com', 'withdrawn', ''],
      ['shop.example.net', 'no-zone', ''],
      ['www.example.com', 'active', ''],
      ['www.example.com', 'conflict', ''],
      ['www.example.info', 'frozen', ''],
    ])
    expect(rows()[0]?.getAttribute('aria-setsize')).toBe('13')
    expect(head('www.example.com', 'qemu/101').textContent).toContain('web-1 qemu/101')
  })

  test('a row opens its chain as a stepper, with the figure of its target', () => {
    render(<ChainList state={st} traffic={tv} />)
    const button = head('www.example.com', 'qemu/101')
    expect(button.getAttribute('aria-expanded')).toBe('false')
    fireEvent.click(button)
    expect(button.getAttribute('aria-expanded')).toBe('true')
    const steps = screen.getByRole('list', { name: 'The chain of www.example.com' })
    expect([...steps.querySelectorAll(':scope > li')].map((li) => `${li.querySelector('.step-name')?.textContent}: ${li.querySelector('.status-word')?.textContent}`)).toEqual([
      'Zone: example.com · served · Main',
      'Edge: Main · 00000000 · verified: yes',
      'Connector: pve1 · active, ready, 4 connections',
      'Path: vmbr0 · VLAN 20 · direct',
      'Target: qemu/101 web-1 · 10.0.0.11:8080 http · level port',
    ])
    expect(document.getElementById(button.getAttribute('aria-controls') ?? '')?.textContent).toContain(
      '2.4 new connections per second from the connector to 10.0.0.11:8080, shared by 2 routes',
    )
    fireEvent.click(button)
    expect(screen.queryByRole('list', { name: 'The chain of www.example.com' })).toBeNull()
  })

  test.each([
    ['held.example.com', 'lxc/200', 'Rule: 503: named in the Notes of lxc/200 but not routed; claim kept'],
    ['example.com', 'qemu/105', 'Hostname policy: the apex of zone example.com is published only when allowHosts names it: add "example.com" to allowHosts'],
    ['www.example.com', 'qemu/102', 'Claim: hostname is held by qemu/101'],
    ['shop.example.net', 'qemu/107', 'Zone: zone example.net is served through no credential'],
    ['new.example.com', 'lxc/201', 'Approval: waits for approval: admission mode approve'],
    ['old.example.com', 'qemu/104', 'Target: qemu/104 db-1 · withdrawn (503): identity check failed'],
    ['api.example.com', 'qemu/103', 'Target: qemu/103 api-1 · 10.0.0.13:9000 http · target is not answering'],
    ['www.example.info', 'qemu/106', 'Rule: account frozen: zone example.info is no longer listed by credential cred1'],
  ])('%s of %s ends with %s', (hostname, owner, last) => {
    render(<ChainList state={st} traffic={tv} />)
    fireEvent.click(head(hostname, owner))
    const items = [...screen.getByRole('list', { name: `The chain of ${hostname}` }).querySelectorAll(':scope > li')]
    const end = items.at(-1)
    expect(`${end?.querySelector('.step-name')?.textContent}: ${end?.querySelector('.status-word')?.textContent}`).toBe(last)
  })

  test('the name of the stepper shows what a hostname hides, as the row does', () => {
    const odd = 'www.example.com\u202e'
    const bidi = { ...st, routes: st.routes.map((r) => (r.owner === 'qemu/101' && r.hostname === 'www.example.com' ? { ...r, hostname: odd } : r)) }
    render(<ChainList state={bidi} traffic={tv} />)
    fireEvent.click(head('www.example.com⟨U+202E⟩', 'qemu/101'))
    expect(screen.getByRole('list', { name: 'The chain of www.example.com⟨U+202E⟩' })).toBeTruthy()
  })

  test('the figures of each trunk on top', () => {
    const { container } = render(<ChainList state={st} traffic={tv} />)
    const strip = within(screen.getByRole('list', { name: 'Traffic between the edge and the connectors' }))
    const last = tv.tunnels[0]?.samples.at(-1)
    expect(strip.getAllByRole('listitem').map((li) => li.textContent)).toEqual([
      `Main · 00000000 ${last?.rps.toFixed(1)} req/s · ${last?.errorsPerSec.toFixed(1)} errors/s connector ready`,
      'Lab · 00000000 4.0 req/s · 0.0 errors/s (no new samples) connector ready',
    ])
    expect(container.querySelector('.trunk-strip .muted.num')).toBeTruthy()
  })

  test('only the routes asked for, the problems first when asked', () => {
    const only = new Set(
      [
        ['www.example.com', 'qemu/101'],
        ['www.example.com', 'qemu/102'],
        ['app.example.com', 'qemu/101'],
      ].map(([hostname = '', owner = '']) => routeKey({ hostname, owner })),
    )
    render(<ChainList state={st} traffic={tv} only={only} problemsFirst />)
    expect(rows().map((li) => `${li.querySelector('.chain-host')?.textContent} ${li.querySelector('.status-word')?.textContent}`)).toEqual([
      'www.example.com conflict',
      'app.example.com active',
      'www.example.com active',
    ])
  })

  test('nothing to show says so', () => {
    render(<ChainList state={st} traffic={tv} only={new Set()} />)
    expect(screen.getByText('No hostname matches.')).toBeTruthy()
  })

  test('a thousand hostnames, a screenful in the page, and one stop of the Tab key', () => {
    const big = scaled({ routes: 1000 })
    render(<ChainList state={big} />)
    const shown = rows()
    expect(shown.length).toBeLessThan(60)
    expect(shown[0]?.getAttribute('aria-setsize')).toBe('1000')
    const stops = shown.map((li) => li.querySelector('button')).filter((b) => b?.tabIndex === 0)
    expect(stops).toHaveLength(1)
    const first = stops[0]
    if (!first) throw new Error('no tab stop')
    first.focus()
    fireEvent.keyDown(first, { key: 'ArrowDown' })
    expect(document.activeElement?.querySelector('.chain-host')?.textContent).toBe('app-0001.example.com')
    fireEvent.keyDown(document.activeElement ?? first, { key: 'End' })
    expect(document.activeElement?.querySelector('.chain-host')?.textContent).toBe('app-0999.example.com')
    expect(document.activeElement?.closest('li')?.getAttribute('aria-posinset')).toBe('1000')
    fireEvent.keyDown(document.activeElement ?? first, { key: 'Home' })
    expect(document.activeElement?.querySelector('.chain-host')?.textContent).toBe('app-0000.example.com')
  })
})
