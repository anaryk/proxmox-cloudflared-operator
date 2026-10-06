import { useState } from 'react'

import type { EventFilter } from '../../app/EventsTable'
import { Button } from '../../components/Button'
import { Field } from '../../components/Field'
import { Untrusted } from '../../components/Untrusted'
import { fromLocalInput, isActive, localInput, splitList } from './filter'

const levels = ['error', 'warn', 'info']

// The kinds the daemon writes today. One it adds later is offered as soon as
// an event of it is on the page.
const knownKinds = ['route', 'conflict', 'action', 'problem', 'claim', 'rollout', 'writer', 'admin', 'credential', 'hold', 'egress', 'connector', 'identity']

function Checks({ options, selected, onChange }: { options: readonly string[]; selected: string[]; onChange: (to: string[]) => void }) {
  return (
    <div className="filter-checks">
      {options.map((o) => (
        <label key={o}>
          <input type="checkbox" checked={selected.includes(o)} onChange={(e) => onChange(e.target.checked ? [...selected, o] : selected.filter((s) => s !== o))} />
          <Untrusted text={o} />
        </label>
      ))}
    </div>
  )
}

// ListInput is a field that takes several values. What is typed is kept as
// typed, so that a comma or a space can be; the address has the values, and
// when it changes by itself the field shows them.
function ListInput({ label, example, values, onChange }: { label: string; example: string; values: string[]; onChange: (to: string[]) => void }) {
  const shown = values.join(', ')
  const [draft, setDraft] = useState({ shown, text: shown })
  if (draft.shown !== shown) setDraft({ shown, text: shown })
  return (
    <Field label={label}>
      {(control) => (
        <input
          {...control}
          type="text"
          placeholder={example}
          value={draft.text}
          spellCheck={false}
          autoComplete="off"
          onChange={(e) => {
            const text = e.target.value
            const next = splitList(text)
            setDraft({ shown: next.join(', '), text })
            onChange(next)
          }}
        />
      )}
    </Field>
  )
}

// EventFilters is the filters of the Events page, which the address holds:
// level, kind, route, guest, account, text and a range of time in the
// browser's own zone.
export function EventFilters({ filter, kinds, onChange }: { filter: EventFilter; kinds: readonly string[]; onChange: (to: EventFilter) => void }) {
  const set = (patch: Partial<EventFilter>) => onChange({ ...filter, ...patch })
  const kind = filter.kind ?? []
  const options = [...new Set([...knownKinds, ...kinds, ...kind])]
  return (
    <form className="card filters" role="search" aria-label="Filter the events" onSubmit={(e) => e.preventDefault()}>
      <div className="filters-row">
        <fieldset className="filter-group">
          <legend>Level</legend>
          <Checks options={levels} selected={filter.level ?? []} onChange={(level) => set({ level })} />
        </fieldset>
        <details className="filter-group filter-kinds">
          <summary>Kind{kind.length > 0 && ` (${kind.length})`}</summary>
          <Checks options={options} selected={kind} onChange={(to) => set({ kind: to })} />
        </details>
      </div>
      <div className="filters-grid">
        <ListInput label="Route" example="www.example.com" values={filter.route ?? []} onChange={(route) => set({ route })} />
        <ListInput label="Guest" example="qemu/101" values={filter.guest ?? []} onChange={(guest) => set({ guest })} />
        <ListInput label="Account" example="the id of the account" values={filter.account ?? []} onChange={(account) => set({ account })} />
        <Field label="Text">
          {(control) => (
            <input {...control} type="search" placeholder="in any field" value={filter.text ?? ''} spellCheck={false} autoComplete="off" onChange={(e) => set({ text: e.target.value })} />
          )}
        </Field>
        <Field label="From">
          {(control) => <input {...control} type="datetime-local" value={localInput(filter.since)} onChange={(e) => set({ since: fromLocalInput(e.target.value) })} />}
        </Field>
        <Field label="To" hint={filter.until ? 'Load older events reads the newest events up to it from the log on the node.' : undefined}>
          {(control) => <input {...control} type="datetime-local" value={localInput(filter.until)} onChange={(e) => set({ until: fromLocalInput(e.target.value, true) })} />}
        </Field>
      </div>
      {isActive(filter) && (
        <div className="filters-foot">
          <Button small onClick={() => onChange({})}>
            Clear the filters
          </Button>
        </div>
      )}
    </form>
  )
}
