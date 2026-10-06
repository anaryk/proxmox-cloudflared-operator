import { useEffect, useId, useRef, useState } from 'react'

import { type AppState, lastCycle, useApp, useStore } from '../api/store'
import { IconButton } from '../components/Button'
import { MarkIcon, MenuIcon, type Tone, ToneIcon, UserIcon, WarnIcon } from '../components/icons'
import { Pill } from '../components/Pill'
import { durationText } from '../text/duration'
import { egressText, inventoryText, modeText, writerText } from '../text/words'
import { type Density, setDensity, setTheme, type Theme, usePreferences } from '../theme/theme'
import { useNow } from './clock'
import { Link } from './Link'
import { navigate } from './router'

export interface PillSpec {
  key: 'mode' | 'inventory' | 'writer' | 'egress' | 'live'
  tone: Tone
  label: string
  value: string
  title?: string
}

const zeroTime = (at?: string) => !at || at.startsWith('0001-01-01T00:00:00')

// pillsOf are the five statuses of the top bar (spec-ui 3.1).
export function pillsOf(s: AppState, now: number): PillSpec[] {
  const st = s.state
  const ran = st !== undefined && !zeroTime(st.at)

  const mode = st ? modeText(st) : 'unknown'
  const modePill: PillSpec =
    mode === 'observe-only'
      ? { key: 'mode', tone: 'info', label: 'Mode', value: 'observe-only' }
      : mode === 'enforce'
        ? { key: 'mode', tone: 'ok', label: 'Mode', value: 'enforcing' }
        : { key: 'mode', tone: 'idle', label: 'Mode', value: mode }

  const inventory = st ? inventoryText(st) : 'unknown'
  const inventoryTone: Tone = inventory === 'complete' ? 'ok' : inventory === 'incomplete' ? 'warn' : 'idle'

  const verdict = ran ? st.writerVerdict : 'unknown'
  const writerTone: Tone = verdict === 'ok' ? 'ok' : verdict === 'unknown' ? (ran ? 'warn' : 'idle') : 'fail'

  const egress = st?.egress?.state ?? ''
  const egressTone: Tone = egress === 'on' ? 'ok' : egress === '' ? 'idle' : 'fail'

  return [
    modePill,
    { key: 'inventory', tone: inventoryTone, label: 'Inventory', value: inventory },
    { key: 'writer', tone: writerTone, label: 'Writer', value: verdict, title: ran ? writerText(verdict) : undefined },
    { key: 'egress', tone: egressTone, label: 'Egress', value: egress || 'not checked yet', title: egressText({ state: egress }) },
    livePill(s, now),
  ]
}

function livePill(s: AppState, now: number): PillSpec {
  const since = s.connSince ? Date.parse(s.connSince) : NaN
  const ago = Number.isFinite(since) ? durationText(now - since) : '-'
  switch (s.conn) {
    case 'web-down':
      return { key: 'live', tone: 'fail', label: 'pco web', value: 'no answer' }
    case 'daemon-down':
      return { key: 'live', tone: 'fail', label: 'Daemon', value: 'no answer' }
    case 'stale':
      return { key: 'live', tone: 'warn', label: 'Stale', value: ago }
    case 'reconnecting':
      return { key: 'live', tone: 'warn', label: 'Reconnecting', value: Number.isFinite(since) ? ago : '…' }
  }
  const finished = lastCycle(s)
  return { key: 'live', tone: 'ok', label: 'Live', value: finished ? durationText(now - Date.parse(finished)) : 'no cycle yet' }
}

const worse: readonly Tone[] = ['fail', 'warn', 'info', 'idle', 'ok']

// summaryOf is the one pill a narrow window shows: the worst of the five.
export function summaryOf(pills: readonly PillSpec[]): PillSpec {
  const worst = [...pills].sort((a, b) => worse.indexOf(a.tone) - worse.indexOf(b.tone))[0]
  if (!worst || worst.tone === 'ok') {
    const mode = pills.find((p) => p.key === 'mode')
    return { key: 'live', tone: 'ok', label: mode?.value ?? 'Live', value: 'live' }
  }
  return worst
}

const methodWords: Readonly<Record<string, string>> = {
  ticket: 'signed in with the Proxmox VE session',
  token: 'signed in with an API token',
  password: 'signed in with a password',
}

function UserMenu({ onAbout }: { onAbout: () => void }) {
  const store = useStore()
  const session = useApp((s) => s.session)
  const prefs = usePreferences()
  const [open, setOpen] = useState(false)
  const box = useRef<HTMLDivElement>(null)
  const id = useId()

  useEffect(() => {
    if (!open) return
    const close = (e: Event) => {
      if (e instanceof KeyboardEvent ? e.key === 'Escape' : !box.current?.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('keydown', close)
    document.addEventListener('pointerdown', close)
    return () => {
      document.removeEventListener('keydown', close)
      document.removeEventListener('pointerdown', close)
    }
  }, [open])

  const themes: [Theme, string][] = [
    ['system', 'System'],
    ['light', 'Light'],
    ['dark', 'Dark'],
  ]
  const densities: [Density, string][] = [
    ['comfortable', 'Comfortable'],
    ['compact', 'Compact'],
  ]
  return (
    <div className="menu" ref={box}>
      <button type="button" className="menu-button" aria-expanded={open} aria-controls={id} onClick={() => setOpen(!open)}>
        <UserIcon />
        <span className="menu-user">{session?.user ?? 'Not signed in'}</span>
        <span className="sr-only">: user menu</span>
      </button>
      <div className="menu-pop" id={id} hidden={!open}>
        {session && (
          <p className="menu-who">
            <b>{session.user}</b>
            <br />
            <span className="muted">
              {session.role} · {methodWords[session.method] ?? session.method}
            </span>
          </p>
        )}
        <p className="menu-head" id={`${id}theme`}>
          Theme
        </p>
        <div className="seg" role="group" aria-labelledby={`${id}theme`}>
          {themes.map(([t, label]) => (
            <button key={t} type="button" aria-pressed={prefs.theme === t} onClick={() => setTheme(t)}>
              {label}
            </button>
          ))}
        </div>
        <p className="menu-head" id={`${id}density`}>
          Density
        </p>
        <div className="seg" role="group" aria-labelledby={`${id}density`}>
          {densities.map(([d, label]) => (
            <button key={d} type="button" aria-pressed={prefs.density === d} onClick={() => setDensity(d)}>
              {label}
            </button>
          ))}
        </div>
        <div className="menu-actions">
          <button
            type="button"
            className="btn btn-small"
            onClick={() => {
              setOpen(false)
              onAbout()
            }}
          >
            About pco
          </button>
          {session && (
            <button
              type="button"
              className="btn btn-small"
              onClick={() => {
                setOpen(false)
                void store.signOut().then(() => navigate('/signin'))
              }}
            >
              Sign out
            </button>
          )}
        </div>
      </div>
    </div>
  )
}

// TopBar is the bar that never scrolls away: the mark, the node, the five
// statuses and the problems, and the user's menu (spec-ui 3.1).
export function TopBar({ navOpen, onNav, onAbout }: { navOpen: boolean; onNav: () => void; onAbout: () => void }) {
  const app = useApp((s) => s)
  const now = useNow()
  const [listOpen, setListOpen] = useState(false)
  const listId = useId()
  const pills = pillsOf(app, now)
  const summary = summaryOf(pills)
  const problems = app.state?.problems.length ?? 0
  const node = app.session?.node || app.state?.node
  const profile = app.session?.profile || app.state?.profile

  return (
    <header className="topbar">
      <span className="navtoggle">
        <IconButton label={navOpen ? 'Close navigation' : 'Open navigation'} icon={<MenuIcon />} aria-expanded={navOpen} aria-controls="nav" onClick={onNav} />
      </span>
      <Link to="/" className="mark" aria-label="pco, overview">
        <MarkIcon />
        <span aria-hidden="true">pco</span>
      </Link>
      {node && (
        <span className="chip" title="Node and profile">
          {node}
          {profile && ` · ${profile}`}
        </span>
      )}
      <div className="pills" role="group" aria-label="Status of pco">
        {pills.map((p) => (
          <span key={p.key} className="pill-wide" title={p.title}>
            <Pill tone={p.tone} label={p.label} value={p.value} />
          </span>
        ))}
        <span className="pill-summary">
          <Pill tone={summary.tone} label={summary.label} value={summary.value} onClick={() => setListOpen(!listOpen)} expanded={listOpen} controls={listId} />
          <ul className="pill-list" id={listId} hidden={!listOpen}>
            {pills.map((p) => (
              <li key={p.key}>
                <ToneIcon tone={p.tone} /> {p.label}: <b>{p.value}</b>
              </li>
            ))}
          </ul>
        </span>
        {problems > 0 && (
          <Link to="/#problems" className="pill pill-warn problems">
            <WarnIcon />
            <b className="pill-value">{problems === 1 ? '1 problem' : `${problems} problems`}</b>
          </Link>
        )}
      </div>
      <span className="spacer" />
      <UserMenu onAbout={onAbout} />
    </header>
  )
}
