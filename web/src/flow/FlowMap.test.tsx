import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import type { State, TrafficView } from '../api/types.gen'
import { collapse } from './collapse'
import flowCss from './flow.css?raw'
import FlowMap, { gestureFilter } from './FlowMap'
import { layout } from './layout'
import { buildModel } from './model'
import { createMotion, dotCap, type MotionLoop } from './motion'
import { extrasOf } from './panel'
import chains from './testdata/chains.json'
import { large, outage, trafficFor, wide } from './testdata/scale'
import traffic from './testdata/traffic.json'
import type { FlowMapProps, MapExtras, Model } from './types'

const st = chains as unknown as State
const tv = traffic as unknown as TrafficView

const viewOf = (s: State, t?: TrafficView): Model => collapse(buildModel(s, t), { expanded: new Set() }).model
const golden = viewOf(st, tv)

// A clock of frames that the test runs by hand, for the real loop.
function clock() {
  let now = 0
  let next: ((t: number) => void) | undefined
  return {
    frame: (cb: (t: number) => void) => {
      next = cb
      return 1
    },
    cancel: () => {
      next = undefined
    },
    run(seconds: number) {
      for (let i = 0; i < Math.round(seconds * 60); i++) {
        now += 1000 / 60
        const cb = next
        next = undefined
        cb?.(now)
      }
    },
  }
}

interface Shown {
  motion: MotionLoop
  tick: (seconds: number) => void
  onSelect: ReturnType<typeof vi.fn>
  onExpand: ReturnType<typeof vi.fn>
  frame: HTMLElement
  rerender: (more: Partial<FlowMapProps>) => void
}

// show draws a view as the Overview would, holding the focus the map moves.
function show(model: Model = golden, more: Partial<FlowMapProps> = {}): Shown {
  const c = clock()
  const motion = createMotion({ cap: dotCap, frame: c.frame, cancel: c.cancel, hidden: () => false, reducedMotion: () => false })
  const onSelect = vi.fn()
  const onExpand = vi.fn()
  const laid = layout(model)
  function Map(props: Partial<FlowMapProps>) {
    const [focus, setFocus] = useState<string>()
    return (
      <FlowMap
        model={model}
        layout={laid}
        motion={motion}
        focus={focus}
        onFocus={setFocus}
        onSelect={onSelect}
        onExpand={onExpand}
        onViewport={() => undefined}
        reducedMotion={false}
        {...props}
      />
    )
  }
  const r = render(<Map {...more} />)
  const frame = screen.getByRole('group', { name: 'Flow map' })
  return { motion, tick: c.run, onSelect, onExpand, frame, rerender: (next) => r.rerender(<Map {...more} {...next} />) }
}

const card = (id: string) => {
  const el = document.querySelector(`[data-node="${id}"]`)
  if (!el) throw new Error(`no card ${id}`)
  return el as HTMLElement
}
const item = (id: string) => {
  const el = [...document.querySelectorAll<HTMLElement>('[data-item]')].find((x) => x.dataset.item === id)
  if (!el) throw new Error(`no item ${id}`)
  return el
}
const line = (id: string) => {
  const el = [...document.querySelectorAll<SVGGElement>('[data-edge]')].find((x) => x.dataset.edge === id)
  if (!el) throw new Error(`no line ${id}`)
  return el
}
const dashOf = (id: string) => line(id).querySelector('.fm-line')?.getAttribute('stroke-dasharray')

const trunk = 'edge:acc1>connector:acc1'
const rogue = 'rogue:0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807'
const webPort = 'path:vmbr0.20>guest:qemu/101|10.0.0.11:8080'

describe('the flow map', () => {
  test('a card for every node, in the look of its kind', () => {
    show()
    expect(document.querySelectorAll('.fm-card')).toHaveLength(golden.nodes.length)
    expect(card('zone:example.com').classList.contains('fm-zone')).toBe(true)
    expect(card('edge:acc1').classList.contains('fm-edge')).toBe(true)
    expect(card('connector:acc1').classList.contains('fm-connector')).toBe(true)
    expect(card(rogue).classList.contains('fm-rogue')).toBe(true)
    expect(card('path:vmbr0.20').classList.contains('fm-path')).toBe(true)
    expect(card('guest:qemu/101').classList.contains('fm-target')).toBe(true)
    expect(card('address:10.0.9.5').classList.contains('fm-target')).toBe(true)
    expect(card('connector:acc1').textContent).toContain('active, ready, 4 connections')
    expect(card(rogue).textContent).toContain('198.51.100.7')
    expect(card('guest:qemu/101').querySelectorAll('.fm-port')).toHaveLength(1)
    expect(card('guest:qemu/103').querySelector('.fm-port')?.getAttribute('data-state')).toBe('unreachable')
  })

  test('every line in its style, with its dashes', () => {
    show()
    expect(line('zone:example.com>edge:acc1').classList.contains('fm-edge-hairline')).toBe(true)
    expect(line('zone:example.com>edge:acc1').textContent).toBe('6 rules')
    expect(line(trunk).querySelector('.fm-pipe')).not.toBeNull()
    expect(line(trunk).querySelectorAll('.fm-lane')).toHaveLength(4)
    expect(line('edge:acc4>connector:acc4').querySelectorAll('.fm-lane')).toHaveLength(2)
    expect(line(trunk).textContent).toContain('38.6 req/s')
    expect(line('edge:acc4>connector:acc4').classList.contains('fm-muted')).toBe(true)
    expect(dashOf('path:vmbr0>guest:qemu/103|10.0.0.13:9000')).toBe('6 4')
    expect(line('path:vmbr0>guest:qemu/103|10.0.0.13:9000').querySelector('.fm-cross')).not.toBeNull()
    expect(dashOf('path:vmbr1.20>guest:qemu/104#withdrawn')).toBe('2 3')
    expect(line('path:vmbr1.20>guest:qemu/104#withdrawn').textContent).toBe('503')
    expect(dashOf(webPort)).toBeNull()
    expect(line(webPort).querySelector('.fm-arrow')).not.toBeNull()
    expect(dashOf(`edge:acc1>${rogue}`)).toBe('4 3')
    expect(line(`edge:acc1>${rogue}`).classList.contains('fm-edge-rogue')).toBe(true)
  })

  test('dots on the trunk and the lines with a figure; never on hostname lines or a rogue connector’s', () => {
    const { motion, tick } = show()
    tick(2)
    expect(motion.dotsOn(trunk)).toBeGreaterThan(0)
    expect(motion.dotsOn(webPort)).toBeGreaterThan(0)
    expect(motion.dotsOn(`edge:acc1>${rogue}`)).toBe(0)
    expect(motion.dotsOn('zone:example.com>edge:acc1')).toBe(0)
    expect(motion.dotsOn('edge:acc4>connector:acc4')).toBe(0)
    expect(document.querySelectorAll('.fm-dots .dot').length).toBeGreaterThan(0)
  })

  test('held routes carry 503 and stop at the connector; rejected routes have no line', () => {
    show()
    const held = item('route:held.example.com lxc/200')
    expect([...held.querySelectorAll('.badge')].map((b) => b.textContent)).toEqual(['503'])
    expect(
      golden.edges
        .filter((e) => e.routes?.includes('held.example.com lxc/200'))
        .map((e) => e.style)
        .sort(),
    ).toEqual(['hairline', 'trunk'])
    const rejected = item('route:example.com qemu/105')
    expect(rejected.dataset.state).toBe('rejected')
    expect(golden.edges.some((e) => e.routes?.includes('example.com qemu/105'))).toBe(false)
    expect(item('route:api.example.com qemu/103').textContent).toContain('DNS')
    expect(item('route:www.example.com qemu/102').textContent).toContain('held by qemu/101 web-1')
  })

  test('each item says what it is', () => {
    show()
    expect(item('route:www.example.com qemu/101').getAttribute('aria-label')).toBe('www.example.com, active, served by qemu/101 web-1 on port 8080')
    expect(item('zone:example.com').getAttribute('aria-label')).toBe('Zone example.com, served, account Main, 10 hostnames')
    expect(item('connector:acc4').getAttribute('aria-label')).toContain('not checked in the last cycle')
    expect(item(rogue).getAttribute('aria-label')).toBe('Connector not run by pco: 198.51.100.7, cloudflared 2026.8.0, Main · 00000000')
    expect(item('guest:qemu/101').getAttribute('aria-label')).toBe('Guest web-1 qemu/101, ports :8080 http active, 2.4 new connections per second, shared by 2 routes')
  })

  test('what the lines past the connector carry is in the names, for the keyboard', () => {
    show()
    expect(item('path:vmbr0.20').getAttribute('aria-label')).toBe('Path vmbr0 · VLAN 20 · direct, 2.4 new connections per second to the targets behind it')
    expect(item('path:vmbr1').getAttribute('aria-label')).toBe('Path vmbr1 · direct, 1.2 new connections per second to the targets behind it, no new samples')
    expect(item('path:vmbr0').getAttribute('aria-label')).toBe('Path vmbr0 · direct')
    act(() => item('path:vmbr0.20').focus())
    expect(screen.getByRole('tooltip').textContent).toBe('2.4 new connections per second to the targets behind it: new connections from the connector, not requests')
    expect(item('path:vmbr0.20').getAttribute('aria-describedby')).toBe(screen.getByRole('tooltip').id)
  })

  test('what a guest or Cloudflare wrote is isolated, its controls shown', () => {
    const odd: Model = { ...golden, nodes: golden.nodes.map((n) => (n.id === 'edge:acc1' ? { ...n, label: 'Main‮evil' } : n)) }
    show(odd)
    expect(card('edge:acc1').querySelector('bdi .cp')?.textContent).toBe('⟨U+202E⟩')
    expect(item('edge:acc1').getAttribute('aria-label')).toContain('Main⟨U+202E⟩evil')
  })

  test('hovering an item lights its chain and dims the rest', () => {
    const { frame } = show()
    expect(frame.querySelector('.fm-dim')).toBeNull()
    fireEvent.pointerOver(item('route:www.example.com qemu/101'))
    for (const id of ['zone:example.com>edge:acc1', trunk, 'connector:acc1>path:vmbr0.20', webPort]) expect(line(id).classList.contains('fm-dim')).toBe(false)
    for (const id of ['path:vmbr0>guest:qemu/103|10.0.0.13:9000', `edge:acc1>${rogue}`, 'edge:acc4>connector:acc4']) expect(line(id).classList.contains('fm-dim')).toBe(true)
    for (const id of ['edge:acc1', 'connector:acc1', 'path:vmbr0.20', 'guest:qemu/101', 'zone:example.com']) expect(card(id).classList.contains('fm-dim')).toBe(false)
    for (const id of ['guest:qemu/103', 'edge:acc4', 'zone:example.dev', rogue]) expect(card(id).classList.contains('fm-dim')).toBe(true)
    expect(item('route:www.example.com qemu/101').classList.contains('fm-dim')).toBe(false)
    expect(item('route:api.example.com qemu/103').classList.contains('fm-dim')).toBe(true)
    fireEvent.pointerLeave(frame)
    expect(frame.querySelector('.fm-dim')).toBeNull()
  })

  test('a click opens the drawer of a card, a line of a card, the trunk or a line to a target', () => {
    const { onSelect, onExpand } = show()
    fireEvent.click(item('guest:qemu/101'))
    fireEvent.click(item('route:www.example.com qemu/101'))
    fireEvent.click(line(trunk).querySelector('.fm-pipe') as Element)
    fireEvent.click(line(webPort).querySelector('.fm-hit') as Element)
    fireEvent.click(line('zone:example.com>edge:acc1').querySelector('.fm-line') as Element)
    fireEvent.click(line('connector:acc1>path:vmbr0.20').querySelector('.fm-hit') as Element)
    expect(onSelect.mock.calls.map((c) => c[0])).toEqual(['guest:qemu/101', 'route:www.example.com qemu/101', trunk, webPort])
    expect(onExpand).not.toHaveBeenCalled()
  })

  test('one tab stop; the arrows move along a chain, Home and End to the first and last column', () => {
    const { frame, onSelect } = show()
    const tabbable = () => [...frame.querySelectorAll<HTMLElement>('[tabindex="0"]')]
    expect(tabbable()).toHaveLength(1)
    const start = item('route:www.example.com qemu/101')
    act(() => start.focus())
    expect(tabbable().map((el) => el.dataset.item)).toEqual(['route:www.example.com qemu/101'])
    const press = (key: string) => act(() => void fireEvent.keyDown(document.activeElement ?? frame, { key }))
    const at = () => (document.activeElement as HTMLElement | null)?.dataset.item
    press('ArrowRight')
    expect(at()).toBe('edge:acc1')
    // the chain the keys follow stays lit, not every chain of the tunnel
    expect(line(webPort).classList.contains('fm-dim')).toBe(false)
    expect(line('path:vmbr0>guest:qemu/103|10.0.0.13:9000').classList.contains('fm-dim')).toBe(true)
    press('ArrowRight')
    press('ArrowRight')
    expect(at()).toBe('path:vmbr0.20')
    press('End')
    expect(at()).toBe('guest:qemu/101')
    expect(tabbable().map((el) => el.dataset.item)).toEqual(['guest:qemu/101'])
    press('Home')
    expect(at()).toBe('route:www.example.com qemu/101')
    press('ArrowDown')
    expect(at()).toBe('route:www.example.com qemu/102')
    press('Enter')
    expect(onSelect).toHaveBeenLastCalledWith('route:www.example.com qemu/102')
    expect(tabbable()).toHaveLength(1)
  })

  test('the renderer drags nothing, and focuses nothing but the items', () => {
    const { frame } = show()
    expect(frame.querySelector('[draggable="true"]')).toBeNull()
    expect(frame.querySelector('svg [tabindex], [data-edge][tabindex]')).toBeNull()
    const focusable = [...frame.querySelectorAll('[tabindex]')]
    expect(focusable.every((el) => el.hasAttribute('data-item'))).toBe(true)
    expect(focusable).toHaveLength(frame.querySelectorAll('[data-item]').length)
  })

  test('reduced motion: chevrons and the figure instead of the dots', () => {
    show(golden, { reducedMotion: true })
    expect(line(trunk).querySelector('.fm-chevrons')?.textContent).toBe('38.6 req/s')
    // a line to a target is short: its figure is on the access point
    expect(line(webPort).querySelector('.fm-chevrons path')).not.toBeNull()
    expect(card('guest:qemu/101').querySelector('.fm-rate')?.textContent).toBe('2.4 conn/s')
    expect(card('guest:qemu/108').querySelector('.fm-rate')).toBeNull()
    expect(line('connector:acc1>path:vmbr0.20').querySelector('.fm-chevrons path')).not.toBeNull()
    expect(line('zone:example.com>edge:acc1').querySelector('.fm-chevrons')).toBeNull()
    expect(line('edge:acc4>connector:acc4').querySelector('.fm-chevrons')).toBeNull()
    expect(line(`edge:acc1>${rogue}`).querySelector('.fm-chevrons')).toBeNull()
  })

  test('a part whose state changed rings once', () => {
    const { rerender } = show()
    expect(document.querySelector('.fm-ring')).toBeNull()
    rerender({ changed: { ids: new Set(['route:old.example.com qemu/104']), n: 1 } })
    expect(item('route:old.example.com qemu/104').querySelector('.fm-ring')).not.toBeNull()
    expect(document.querySelectorAll('.fm-ring')).toHaveLength(1)
  })

  test('a connector pco does not run, and one whose token Cloudflare refuses, carry the rotate command in their tooltip', () => {
    // A real account id, which the command takes.
    const account = '0123456789abcdef0123456789abcdef'
    const named = JSON.parse(JSON.stringify(st).replaceAll('"acc1"', `"${account}"`)) as State
    const refused: State = { ...named, connectors: named.connectors.map((c, i) => (i === 0 ? { ...c, tokenRefused: true } : c)) }
    const view = viewOf(refused, tv)
    const extras: MapExtras = extrasOf(refused, tv, view)
    show(view, { extras })
    expect(card(`connector:${account}`).classList.contains('fm-refused')).toBe(true)
    fireEvent.pointerOver(item(rogue))
    expect(screen.getByRole('tooltip').textContent).toContain(`pco tunnel rotate --account ${account}`)
    fireEvent.pointerOver(item(`connector:${account}`))
    expect(screen.getByRole('tooltip').textContent).toContain('Cloudflare refuses the token')
    expect(screen.getByRole('tooltip').textContent).toContain(`pco tunnel rotate --account ${account}`)
    expect(document.querySelector('button')).toBeNull()
    // an account id of an unexpected form gets no command, and says why
    fireEvent.pointerLeave(document.querySelector('.fm-frame') as Element)
    cleanup()
    show(golden, { extras: extrasOf(st, tv, golden) })
    fireEvent.pointerOver(item(rogue))
    expect(screen.getByRole('tooltip').textContent).toContain('No command: the account id has an unexpected form')
  })

  test('a line past the connector says what its figure counts', () => {
    show()
    fireEvent.pointerOver(line(webPort).querySelector('.fm-hit') as Element)
    expect(screen.getByRole('tooltip').textContent).toBe('2.4 new connections per second: new connections from the connector, not requests, shared by 2 routes')
    fireEvent.pointerOver(line('connector:acc1>path:none').querySelector('.fm-hit') as Element)
    expect(screen.getByRole('tooltip').textContent).toBe('0.6 new connections per second: new connections from the connector, not requests')
    fireEvent.pointerOver(line(trunk).querySelector('.fm-pipe') as Element)
    expect(screen.getByRole('tooltip').textContent).toBe('Traffic between the edge and the connector: 38.6 req/s · 0.1 errors/s')
  })

  test('the tooltip opens right under its item, stays while the pointer is in it and when pressed, and goes after the pointer leaves', async () => {
    const { frame } = show(golden, { extras: extrasOf(st, tv, golden) })
    const box = layout(golden).nodes.get(rogue)
    fireEvent.pointerOver(item(rogue))
    const tip = screen.getByRole('tooltip')
    expect(tip.style.top).toBe(`${(box?.y ?? 0) + (box?.height ?? 0)}px`)
    fireEvent.pointerOver(tip)
    expect(screen.queryByRole('tooltip')).not.toBeNull()
    act(() => item(rogue).focus())
    act(() => tip.focus())
    expect(screen.queryByRole('tooltip')).not.toBeNull()
    expect(flowCss).toMatch(/\.fm-tip \{[^}]*user-select: text;/)
    act(() => item(rogue).focus())
    fireEvent.pointerOver(frame)
    expect(screen.queryByRole('tooltip')).not.toBeNull()
    act(() => (document.activeElement as HTMLElement).blur())
    fireEvent.pointerOver(item(rogue))
    fireEvent.pointerOver(frame)
    // a moment to reach it
    expect(screen.queryByRole('tooltip')).not.toBeNull()
    await act(() => new Promise((r) => setTimeout(r, 300)))
    expect(screen.queryByRole('tooltip')).toBeNull()
  })

  test.each([
    ['large', large()],
    ['outage', outage()],
    ['wide', wide()],
  ])('the %s scenario stays within 150 cards and 400 lines', (_, s) => {
    show(viewOf(s, trafficFor(s)))
    expect(document.querySelectorAll('.fm-card').length).toBeLessThanOrEqual(150)
    expect(document.querySelectorAll('[data-edge]').length).toBeLessThanOrEqual(400)
    expect(document.querySelectorAll('.fm-card').length).toBeGreaterThan(5)
  })
})

// A frame the test sizes: the ResizeObserver it is given is called by hand.
function sized() {
  let fire: () => void = () => undefined
  vi.stubGlobal(
    'ResizeObserver',
    class {
      constructor(cb: () => void) {
        fire = cb
      }
      observe() {}
      disconnect() {}
    },
  )
  return (frame: HTMLElement, width: number, height = 600) => {
    Object.defineProperty(frame, 'clientWidth', { configurable: true, get: () => width })
    Object.defineProperty(frame, 'clientHeight', { configurable: true, get: () => height })
    act(() => fire())
  }
}

const scaleOf = () => Number(/scale\(([\d.]+)\)/.exec((document.querySelector('.fm-view') as HTMLElement).style.transform)?.[1])

function wideScreen(wide: boolean) {
  vi.spyOn(window, 'matchMedia').mockImplementation(
    (q: string) => ({ matches: q === '(min-width: 1200px)' ? wide : false, media: q, addEventListener: () => undefined, removeEventListener: () => undefined }) as unknown as MediaQueryList,
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('touch, zoom and fit', () => {
  test('one finger scrolls the page; two fingers, the mouse and Ctrl with the wheel move the map', () => {
    expect(flowCss).toMatch(/\.fm-frame \{[^}]*touch-action: pan-y;/)
    const ev = (type: string, props: object) => Object.assign(new Event(type), props)
    expect(gestureFilter(ev('touchstart', { touches: { length: 1 } }))).toBe(false)
    expect(gestureFilter(ev('touchstart', { touches: { length: 2 } }))).toBe(true)
    expect(gestureFilter(ev('wheel', { deltaY: 10, ctrlKey: false }))).toBe(false)
    expect(gestureFilter(ev('wheel', { deltaY: 10, ctrlKey: true }))).toBe(true)
    expect(gestureFilter(ev('mousedown', { button: 0, ctrlKey: false }))).toBe(true)
    expect(gestureFilter(ev('mousedown', { button: 2, ctrlKey: false }))).toBe(false)
    expect(gestureFilter(ev('mousedown', { button: 0, ctrlKey: true }))).toBe(false)
    // in the tooltip and on the mini map the map does not pan
    for (const kind of ['fm-tip', 'fm-mini']) {
      const outer = document.createElement('div')
      outer.className = kind
      const inner = document.createElement('span')
      outer.append(inner)
      const down = ev('mousedown', { button: 0, ctrlKey: false })
      Object.defineProperty(down, 'target', { value: inner })
      expect(gestureFilter(down)).toBe(false)
    }
  })

  test('fit takes the width, never so small that the type goes under 11 px; the zoom buttons keep their zoom when the width changes', () => {
    const size = sized()
    const { frame, rerender } = show()
    size(frame, 1186)
    expect(scaleOf()).toBeCloseTo(1186 / 1244, 4)
    size(frame, 900)
    expect(scaleOf()).toBeCloseTo(11 / 12, 4)
    rerender({ zoom: { to: 'in', n: 1 } })
    const zoomed = scaleOf()
    expect(zoomed).toBeCloseTo((11 / 12) * 1.4, 4)
    size(frame, 1186)
    expect(scaleOf()).toBeCloseTo(zoomed, 4)
    rerender({ zoom: { to: 'fit', n: 2 } })
    expect(scaleOf()).toBeCloseTo(1186 / 1244, 4)
  })

  test('the map type is at least 12 px, so that the fit keeps it at 11', () => {
    const sizes = [...flowCss.matchAll(/font(?:-size)?:[^;]*?(\d+(?:\.\d+)?)px/g)].map((m) => Number(m[1]))
    const map = flowCss.slice(0, flowCss.indexOf('/* the tooltip of the map */'))
    const inMap = [...map.matchAll(/font(?:-size)?:[^;]*?(\d+(?:\.\d+)?)px/g)].map((m) => Number(m[1]))
    expect(sizes.length).toBeGreaterThan(0)
    expect(Math.min(...inMap)).toBeGreaterThanOrEqual(12)
  })
})

describe('the mini map', () => {
  const many = () => viewOf(wide(), trafficFor(wide()))

  test('above 40 cards on a wide screen: every card, the view, and nothing for the keyboard or a screen reader', () => {
    wideScreen(true)
    const model = many()
    expect(model.nodes.length).toBeGreaterThan(40)
    const { rerender } = show(model)
    const mini = document.querySelector('.fm-mini')
    expect(mini?.getAttribute('aria-hidden')).toBe('true')
    expect(mini?.querySelectorAll('.fm-mini-node')).toHaveLength(model.nodes.length)
    expect(mini?.querySelector('[tabindex]')).toBeNull()
    const view = mini?.querySelector('.fm-mini-view')
    const before = Number(view?.getAttribute('width'))
    expect(before).toBeGreaterThan(0)
    rerender({ zoom: { to: 'in', n: 1 } })
    expect(Number(view?.getAttribute('width'))).toBeCloseTo(before / 1.4, 3)
  })

  test('a press on it brings that part of the map into view', () => {
    wideScreen(true)
    const { rerender } = show(many())
    rerender({ zoom: { to: 'in', n: 1 } })
    rerender({ zoom: { to: 'in', n: 2 } })
    const before = (document.querySelector('.fm-view') as HTMLElement).style.transform
    const mini = document.querySelector('.fm-mini') as Element
    fireEvent.pointerDown(mini, { clientX: 150, clientY: 150, button: 0, pointerId: 1 })
    fireEvent.pointerUp(mini, { pointerId: 1 })
    expect((document.querySelector('.fm-view') as HTMLElement).style.transform).not.toBe(before)
  })

  test('none at 40 cards or fewer, or on a narrow screen', () => {
    wideScreen(true)
    show()
    expect(document.querySelector('.fm-mini')).toBeNull()
    cleanup()
    wideScreen(false)
    show(many())
    expect(document.querySelector('.fm-mini')).toBeNull()
  })
})
