import { describe, expect, test } from 'vitest'

import type { FlowEdge, FlowNode, FlowRow } from '../flow/types'
import { countersWhy, edgeTip, nodeLabel, rowLabel, trunkFigure } from './flow'

const row = (r: Partial<FlowRow>): FlowRow => ({ hostname: 'www.example.com', owner: 'qemu/101', state: 'active', tags: [], ...r })

describe('the names of the map', () => {
  test('a hostname: its state, why, its tags, and who serves it on which port', () => {
    expect(rowLabel(row({ guest: 'web-1' }), '8080')).toBe('www.example.com, active, served by qemu/101 web-1 on port 8080')
    expect(rowLabel(row({ state: 'unreachable', reason: 'target is not answering', tags: ['DNS'] }))).toBe(
      'www.example.com, unreachable: target is not answering, a DNS record of someone else holds the name, served by qemu/101',
    )
    expect(rowLabel(row({ state: 'held', tags: ['503'] }))).toBe('www.example.com, held, answers 503, served by qemu/101')
    expect(rowLabel(row({ state: 'conflict', holder: 'qemu/102 web-2' }))).toBe('www.example.com, conflict, held by qemu/102 web-2, asked for by qemu/101')
    expect(rowLabel(row({ kind: 'unapproved', state: 'unapproved', tags: ['waits for approval'] }))).toBe('www.example.com, waits for approval, asked for by qemu/101')
    expect(rowLabel(row({ state: 'rejected', reason: 'add example.com to allowHosts' }))).toBe('www.example.com, rejected: add example.com to allowHosts, asked for by qemu/101')
    expect(rowLabel(row({ kind: 'more', label: '+ 186 active' }))).toBe('+ 186 active, opens the rest of the card')
    expect(rowLabel(row({ kind: 'group', label: '214 unreachable: no answer on port 8080' }))).toBe('214 unreachable: no answer on port 8080, lists their routes')
  })

  test('what a guest wrote is named with its controls marked', () => {
    expect(rowLabel(row({ hostname: 'www.example.com‮', guest: 'evil\u0007' }))).toBe('www.example.com⟨U+202E⟩, active, served by qemu/101 evil⟨U+0007⟩')
  })

  test('the cards', () => {
    const node = (n: Partial<FlowNode>): FlowNode => ({ id: 'x', band: 'path', kind: 'path', label: 'vmbr0 · direct', ...n })
    expect(nodeLabel(node({}))).toBe('Path vmbr0 · direct')
    expect(nodeLabel(node({ id: 'no-zone', band: 'hostnames', kind: 'zone', label: 'No zone', rows: [row({}), row({ owner: 'qemu/102' })] }))).toBe('Hostnames in no zone, 2 hostnames')
    expect(nodeLabel(node({ band: 'hostnames', kind: 'zone', label: 'example.com', ref: 'example.com', state: 'served', lines: ['Main'], counts: { active: 600, unreachable: 3 } }))).toBe(
      'Zone example.com, served, account Main, 600 active, 3 unreachable',
    )
    expect(nodeLabel(node({ band: 'edge', kind: 'edge', label: 'Main · 3f2a91c0', state: 'yes', lines: ['4 connections'] }))).toBe('Edge of tunnel Main · 3f2a91c0, verified: yes, 4 connections')
    expect(nodeLabel(node({ band: 'targets', kind: 'group', label: '38 guests on vmbr0', state: 'active' }))).toBe('38 guests on vmbr0, active, opens them')
    expect(nodeLabel(node({ id: 'address:10.0.9.5', band: 'targets', kind: 'target', label: '10.0.9.5', lines: ['manual/m1'], ports: [{ id: 'p', label: ':80 http', state: 'active', rate: 0.6, stale: true }] }))).toBe(
      'Address 10.0.9.5 of manual/m1, ports :80 http active, 0.6 new connections per second, no new samples',
    )
  })

  test('the figures of the lines', () => {
    const edge = (e: Partial<FlowEdge>): FlowEdge => ({ id: 'e', from: 'a', to: 'b', style: 'plain', ...e })
    expect(trunkFigure({ rate: 38.64, errors: 0.5 })).toBe('38.6 req/s · 0.5 errors/s')
    expect(trunkFigure({ rate: 4 })).toBe('4.0 req/s')
    expect(edgeTip(edge({ rate: 2.4 }), 2)).toBe('2.4 new connections per second: new connections from the connector, not requests, shared by 2 routes')
    expect(edgeTip(edge({ rate: 1.2, stale: true }), 1)).toBe('1.2 new connections per second: new connections from the connector, not requests, no new samples')
    expect(edgeTip(edge({ style: 'withdrawn' }), 1)).toBe('Withdrawn: the rule answers 503 and the DNS record is kept')
  })

  test('why there are no figures per target, and what went wrong in a failed read', () => {
    expect(countersWhy('the egress filter is not loaded')).toEqual({ short: 'the egress filter is not loaded' })
    expect(countersWhy('the counters could not be read: nft: signal: killed')).toEqual({ short: 'the counters could not be read', detail: 'nft: signal: killed' })
  })
})
