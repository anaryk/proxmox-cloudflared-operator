import { type KeyboardEvent, type ReactNode, useId } from 'react'

export interface TabItem {
  id: string
  label: string
  // What needs a person in that tab, shown next to its label.
  count?: number
}

// Tabs is a list of tabs and the panel of the selected one. The tabs are one
// stop of the Tab key; the arrow keys, Home and End select another.
export function Tabs({
  label,
  tabs,
  selected,
  onSelect,
  children,
}: {
  label: string
  tabs: readonly TabItem[]
  selected: string
  onSelect: (id: string) => void
  children: ReactNode
}) {
  const base = useId()
  const tabId = (id: string) => `${base}tab-${id}`
  const panelId = `${base}panel`

  const target = (key: string, at: number): number | undefined => {
    switch (key) {
      case 'ArrowRight':
        return at + 1
      case 'ArrowLeft':
        return at - 1
      case 'Home':
        return 0
      case 'End':
        return tabs.length - 1
    }
    return undefined
  }

  const move = (e: KeyboardEvent<HTMLButtonElement>, at: number) => {
    const to = target(e.key, at)
    if (to === undefined) return
    e.preventDefault()
    const tab = tabs[(to + tabs.length) % tabs.length]
    if (!tab) return
    onSelect(tab.id)
    document.getElementById(tabId(tab.id))?.focus()
  }

  return (
    <div className="tabs-wrap">
      <div role="tablist" aria-label={label} className="tabs">
        {tabs.map((tab, at) => {
          const on = tab.id === selected
          return (
            <button
              key={tab.id}
              type="button"
              role="tab"
              id={tabId(tab.id)}
              className="tab"
              aria-selected={on}
              aria-controls={on ? panelId : undefined}
              tabIndex={on ? 0 : -1}
              onClick={() => onSelect(tab.id)}
              onKeyDown={(e) => move(e, at)}
            >
              {tab.label}
              {tab.count ? <span className="tab-count">{tab.count}</span> : null}
            </button>
          )
        })}
      </div>
      <div role="tabpanel" id={panelId} aria-labelledby={tabId(selected)} className="tabpanel">
        {children}
      </div>
    </div>
  )
}
