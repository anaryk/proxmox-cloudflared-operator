import type { ReactNode } from 'react'

// The icons of the interface, drawn on a grid of 16 px with a stroke of
// 1.5 px in the colour of the text around them. An icon is decoration
// unless it has a label; status is never shown by an icon alone.

export interface IconProps {
  label?: string
  className?: string
}

function icon(name: string, shape: ReactNode) {
  function Icon({ label, className }: IconProps) {
    const named = label ? { role: 'img', 'aria-label': label } : { 'aria-hidden': true }
    return (
      <svg
        className={className ? `i ${className}` : 'i'}
        viewBox="0 0 16 16"
        width="16"
        height="16"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        focusable="false"
        {...named}
      >
        {shape}
      </svg>
    )
  }
  Icon.displayName = name
  return Icon
}

const ring = <circle cx="8" cy="8" r="6.4" />

// State
export const OkIcon = icon('OkIcon', <>{ring}<path d="M5.1 8.3l2 2 3.9-4.3" strokeWidth="1.6" /></>)
export const FailIcon = icon('FailIcon', <>{ring}<path d="M5.7 5.7l4.6 4.6M10.3 5.7l-4.6 4.6" strokeWidth="1.6" /></>)
export const WarnIcon = icon('WarnIcon', <><path d="M8 1.9l6.5 11.6H1.5z" /><path d="M8 6.2v3.4M8 11.5v.1" strokeWidth="1.7" /></>)
export const WithdrawnIcon = icon(
  'WithdrawnIcon',
  <><path d="M8 1.6l5.1 2v4.2c0 3.1-2.1 5.3-5.1 6.5-3-1.2-5.1-3.4-5.1-6.5V3.6z" /><path d="M3 3l10 10" /></>,
)
export const HeldIcon = icon('HeldIcon', <>{ring}<path d="M6.4 5.6v4.8M9.6 5.6v4.8" strokeWidth="1.6" /></>)
export const ConflictIcon = icon('ConflictIcon', <path d="M2.5 5.2h9M9 2.7l2.5 2.5L9 7.7M13.5 10.8h-9M7 8.3l-2.5 2.5L7 13.3" />)
export const NoZoneIcon = icon('NoZoneIcon', <circle cx="8" cy="8" r="6.4" strokeDasharray="2.4 2.2" strokeLinecap="butt" />)
export const WaitIcon = icon('WaitIcon', <>{ring}<path d="M8 4.6V8l2.4 1.7" /></>)
export const InfoIcon = icon('InfoIcon', <>{ring}<path d="M8 7.3v4M8 4.8v.1" strokeWidth="1.7" /></>)
export const SkipIcon = icon('SkipIcon', <><circle cx="8" cy="8" r="6.4" strokeDasharray="2 2" strokeLinecap="butt" /><path d="M5.5 8h5" /></>)
export const FrozenIcon = icon(
  'FrozenIcon',
  <path d="M8 1.7v12.6M2.5 4.9l11 6.2M2.5 11.1l11-6.2M6.3 4.2L8 2.5l1.7 1.7M10.5 4.6l2.3.6-.6 2.4M12.2 8.4l.6 2.4-2.3.6M9.7 11.8L8 13.5l-1.7-1.7M5.5 11.4l-2.3-.6.6-2.4M3.8 7.6l-.6-2.4 2.3-.6" />,
)
export const RejectedIcon = icon('RejectedIcon', <>{ring}<path d="M3.5 3.5l9 9" /></>)
export const RogueIcon = icon(
  'RogueIcon',
  <path d="M6 1.8v2.7M10 1.8v2.7M4.2 4.5h7.6v2.2a3.8 3.8 0 0 1-7.6 0zM8 10.5v3.7M11 11.2l3 3M14 11.2l-3 3" />,
)

// Tools
export const MenuIcon = icon('MenuIcon', <path d="M2.5 4h11M2.5 8h11M2.5 12h11" />)
export const SunIcon = icon(
  'SunIcon',
  <><circle cx="8" cy="8" r="3" /><path d="M8 1.5v1.6M8 12.9v1.6M1.5 8h1.6M12.9 8h1.6M3.4 3.4l1.1 1.1M11.5 11.5l1.1 1.1M3.4 12.6l1.1-1.1M11.5 4.5l1.1-1.1" /></>,
)
export const MoonIcon = icon('MoonIcon', <path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z" />)
export const SearchIcon = icon('SearchIcon', <><circle cx="7" cy="7" r="4.5" /><path d="M10.4 10.4l3.3 3.3" /></>)
export const PauseIcon = icon('PauseIcon', <path d="M5.5 3.5v9M10.5 3.5v9" strokeWidth="1.8" />)
export const PlayIcon = icon('PlayIcon', <path d="M5 3.3l7.5 4.7L5 12.7z" fill="currentColor" stroke="none" />)
export const ListIcon = icon('ListIcon', <path d="M5.5 4h8M5.5 8h8M5.5 12h8M2.5 4h.1M2.5 8h.1M2.5 12h.1" strokeWidth="1.6" />)
export const GraphIcon = icon(
  'GraphIcon',
  <g strokeWidth="1.3" strokeLinecap="butt">
    <rect x="1.5" y="3" width="4" height="3.5" rx=".8" />
    <rect x="10.5" y="1.5" width="4" height="3.5" rx=".8" />
    <rect x="10.5" y="10.5" width="4" height="3.5" rx=".8" />
    <path d="M5.5 4.8h2.5c1 0 1.5.5 1.5 1.5v4.5c0 .7.4 1.2 1 1.2M8 4.8c1 0 1.5-.6 2.5-1.5" />
  </g>,
)
export const RefreshIcon = icon('RefreshIcon', <path d="M13 8a5 5 0 1 1-1.5-3.6M13 2.5v3.2H9.8" />)
export const CopyIcon = icon(
  'CopyIcon',
  <g strokeWidth="1.4">
    <rect x="5.5" y="5.5" width="8" height="8" rx="1.2" />
    <path d="M10.5 5.5v-2a1 1 0 0 0-1-1h-6a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h2" />
  </g>,
)
export const CloseIcon = icon('CloseIcon', <path d="M4 4l8 8M12 4l-8 8" strokeWidth="1.6" />)
export const UserIcon = icon('UserIcon', <><circle cx="8" cy="5.5" r="2.8" /><path d="M2.8 14c.6-2.7 2.7-4.2 5.2-4.2s4.6 1.5 5.2 4.2" /></>)

// Navigation
export const OverviewIcon = icon(
  'OverviewIcon',
  <g strokeWidth="1.4">
    <circle cx="3" cy="8" r="1.8" />
    <circle cx="13" cy="3.5" r="1.8" />
    <circle cx="13" cy="12.5" r="1.8" />
    <path d="M4.7 7.3l6.6-3M4.7 8.7l6.6 3" />
  </g>,
)
export const RouteIcon = icon('RouteIcon', <path d="M2 12.5h3.5c1.5 0 2-.8 2.5-2.5l1-4c.4-1.7 1-2.5 2.5-2.5H14M11.5 1.3l2.5 2.2-2.5 2.2" />)
export const GuestIcon = icon('GuestIcon', <><rect x="2" y="2.5" width="12" height="8.5" rx="1.2" /><path d="M5.5 14h5M8 11v3" /></>)
export const NetworkIcon = icon(
  'NetworkIcon',
  <g strokeWidth="1.4">
    <rect x="6" y="1.8" width="4" height="3.4" rx=".6" />
    <rect x="1.5" y="10.8" width="4" height="3.4" rx=".6" />
    <rect x="10.5" y="10.8" width="4" height="3.4" rx=".6" />
    <path d="M8 5.2v2.8M3.5 10.8V8h9v2.8" />
  </g>,
)
export const KeyIcon = icon('KeyIcon', <><circle cx="5" cy="10.5" r="2.8" /><path d="M7 8.5l6-6M11 4.5l1.8 1.8M9.4 6.1l1.4 1.4" /></>)
export const GlobeIcon = icon(
  'GlobeIcon',
  <>
    <circle cx="8" cy="8" r="6.3" strokeWidth="1.4" />
    <path d="M1.8 8h12.4M8 1.7c1.8 1.8 2.6 3.9 2.6 6.3S9.8 12.5 8 14.3C6.2 12.5 5.4 10.4 5.4 8S6.2 3.5 8 1.7z" strokeWidth="1.3" />
  </>,
)
export const TunnelIcon = icon('TunnelIcon', <path d="M2 13.5V8a6 6 0 0 1 12 0v5.5M5.5 13.5V8.5a2.5 2.5 0 0 1 5 0v5" />)
export const EventsIcon = icon('EventsIcon', <path d="M3 3.5h10M3 8h10M3 12.5h6" />)
export const DoctorIcon = icon('DoctorIcon', <path d="M1.5 8.5h3l1.5-4 3 8 1.8-5 1 1h2.7" />)
export const SettingsIcon = icon(
  'SettingsIcon',
  <><circle cx="8" cy="8" r="2.2" /><path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M3.4 12.6l1.4-1.4M11.2 4.8l1.4-1.4" /></>,
)
export const SetupIcon = icon('SetupIcon', <path d="M3 13l7.5-7.5M9 3.5l3.5 3.5M11 2l3 3-1.5 1.5-3-3z" />)

// The product mark, in the colour of traffic.
export function MarkIcon() {
  return (
    <svg className="mark-icon" viewBox="0 0 22 22" width="22" height="22" aria-hidden="true" focusable="false">
      <rect className="mark-tile" x="1" y="1" width="20" height="20" rx="5" />
      <circle className="mark-dot" cx="6.5" cy="11" r="2.3" />
      <path className="mark-arrow" d="M9.5 11h6.2M13 7.8l3.2 3.2-3.2 3.2" />
    </svg>
  )
}

// The tones of status: each has its colour, its icon and, wherever it is
// shown, a word.
export type Tone = 'ok' | 'warn' | 'fail' | 'info' | 'idle'

const toneIcons = { ok: OkIcon, warn: WarnIcon, fail: FailIcon, info: InfoIcon, idle: InfoIcon } as const

export function ToneIcon({ tone, ...props }: IconProps & { tone: Tone }) {
  const Icon = toneIcons[tone]
  return <Icon {...props} />
}
