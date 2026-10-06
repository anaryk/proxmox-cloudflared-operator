import type { ComponentType, ReactNode } from 'react'

import {
  ConflictIcon,
  FailIcon,
  FrozenIcon,
  HeldIcon,
  type IconProps,
  InfoIcon,
  NoZoneIcon,
  OkIcon,
  RejectedIcon,
  SkipIcon,
  type Tone,
  ToneIcon,
  WarnIcon,
  WithdrawnIcon,
} from './icons'
import { Untrusted } from './Untrusted'

interface Look {
  tone: Tone
  Icon: ComponentType<IconProps>
}

const lookOf = (table: Readonly<Record<string, Look>>, key: string): Look | undefined => (Object.hasOwn(table, key) ? table[key] : undefined)

// StatusBadge is a status in its colour, with its icon and a word: never
// the colour alone.
export function StatusBadge({ tone, icon, children }: { tone: Tone; icon?: ReactNode; children: ReactNode }) {
  return (
    <span className={`status status-${tone}`}>
      {icon ?? <ToneIcon tone={tone} />}
      <span className="status-word">{children}</span>
    </span>
  )
}

// The look of each state of a route. A state of a newer daemon is shown in
// its own words, as unknown.
const routeStates: Readonly<Record<string, Look>> = {
  active: { tone: 'ok', Icon: OkIcon },
  unreachable: { tone: 'fail', Icon: FailIcon },
  withdrawn: { tone: 'warn', Icon: WithdrawnIcon },
  conflict: { tone: 'fail', Icon: ConflictIcon },
  'no-zone': { tone: 'idle', Icon: NoZoneIcon },
  held: { tone: 'idle', Icon: HeldIcon },
  // a refusal of the settings, not an error
  rejected: { tone: 'warn', Icon: RejectedIcon },
  frozen: { tone: 'idle', Icon: FrozenIcon },
}

export function StateBadge({ state }: { state: string }) {
  const look = lookOf(routeStates, state)
  if (!look) {
    return (
      <StatusBadge tone="idle" icon={<InfoIcon />}>
        <Untrusted text={state} />
      </StatusBadge>
    )
  }
  return (
    <StatusBadge tone={look.tone} icon={<look.Icon />}>
      {state}
    </StatusBadge>
  )
}

// The levels of a step of the doctor or the diagnosis, and of an event.
const levels: Readonly<Record<string, Look>> = {
  ok: { tone: 'ok', Icon: OkIcon },
  info: { tone: 'info', Icon: InfoIcon },
  warn: { tone: 'warn', Icon: WarnIcon },
  fail: { tone: 'fail', Icon: FailIcon },
  error: { tone: 'fail', Icon: FailIcon },
  skipped: { tone: 'idle', Icon: SkipIcon },
}

export function LevelBadge({ level, children }: { level: string; children?: ReactNode }) {
  const look = lookOf(levels, level)
  if (!look) {
    return (
      <StatusBadge tone="idle" icon={<InfoIcon />}>
        {children ?? <Untrusted text={level} />}
      </StatusBadge>
    )
  }
  return (
    <StatusBadge tone={look.tone} icon={<look.Icon />}>
      {children ?? level}
    </StatusBadge>
  )
}
