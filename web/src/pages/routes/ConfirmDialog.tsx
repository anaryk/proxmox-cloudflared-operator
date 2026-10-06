import { type JSX, useReducer, useState } from 'react'

import { api, ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { Action, ApplyResult, Waiting } from '../../api/types.gen'
import { Badge } from '../../components/Badge'
import { Banner } from '../../components/Banner'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { Field } from '../../components/Field'
import { FailIcon } from '../../components/icons'
import { Untrusted } from '../../components/Untrusted'
import { unaffected } from '../../text/words'
import { canSend, confirmWord, type ConfirmState, effect, type Line, needsWord, reduce, request, start } from './confirm'
import { errorMessage } from './parts'

const noWaiting: Waiting[] = []

// WaitingList is every entry of what waits and every one of its items: the
// dialog never cuts the list it confirms.
export function WaitingList({ items }: { items: readonly Waiting[] }) {
  return (
    <ul className="waiting-list">
      {items.map((w) => (
        <li key={`${w.kind}\u0000${w.subject}\u0000${w.detail}`}>
          <Untrusted text={w.detail} />
          {(w.items ?? []).length > 0 && (
            <ul className="waiting-items mono">
              {w.items.map((item) => (
                <li key={item}>
                  <Untrusted text={item} />
                </li>
              ))}
            </ul>
          )}
        </li>
      ))}
    </ul>
  )
}

export function UnaffectedList({ actions }: { actions: readonly Action[] }) {
  if (actions.length === 0) return null
  return (
    <div className="unaffected">
      <p>Destructive actions that are pending; a confirmation does not affect them:</p>
      <ul className="plain-list">
        {actions.map((a) => (
          <li key={`${a.kind}\u0000${a.target}`}>
            <span className="mono">
              <Untrusted text={a.kind} /> <Untrusted text={a.target} />
            </span>
            {a.detail && (
              <>
                {' '}
                <Untrusted text={a.detail} />
              </>
            )}
            {a.held && (
              <span className="muted">
                {' '}
                (held: <Untrusted text={a.held} />)
              </span>
            )}
          </li>
        ))}
      </ul>
    </div>
  )
}

function Lines({ lines, mark }: { lines: readonly Line[]; mark: 'added' | 'removed' }) {
  if (lines.length === 0) return null
  return (
    <ul className={`diff-list diff-${mark}`}>
      {lines.map((l) => (
        <li key={`${l.kind}\u0000${l.subject}\u0000${l.text}`}>
          <Badge tone={mark === 'added' ? 'warn' : 'idle'}>{mark}</Badge> <Untrusted text={l.text} />
        </li>
      ))}
    </ul>
  )
}

// ConfirmDialog confirms what waits for a confirmation, as pco apply
// --confirm-deletes does: the list of one state, with the offer of that
// state. When the stream brings another offer while it is open, it shows
// what changed and sends nothing until the new list is reviewed.
export function ConfirmDialog(props: { onClose(): void }): JSX.Element {
  const st = useApp((s) => s.state)
  const offer = st?.offer ?? ''
  const waiting = st?.waiting ?? noWaiting
  const [s, dispatch] = useReducer(reduce, undefined, () => start(offer, waiting))
  // Each state the stream brings is put to the machine, once.
  const [seen, setSeen] = useState({ offer, waiting })
  if (seen.offer !== offer || seen.waiting !== waiting) {
    setSeen({ offer, waiting })
    dispatch({ type: 'state', offer, waiting })
  }

  const send = async (now: ConfirmState) => {
    const next = reduce(now, { type: 'send' })
    if (next.name !== 'sending') return
    dispatch({ type: 'send' })
    try {
      const result = await api<ApplyResult>('POST', '/api/v1/apply', request(next))
      dispatch({ type: 'answer', result })
    } catch (e) {
      if (e instanceof ApiError && e.code === 'refused') dispatch({ type: 'refused', message: e.message })
      else dispatch({ type: 'failed', message: errorMessage(e) })
    }
  }
  const review = () => dispatch({ type: 'review', offer, waiting })

  let body: JSX.Element
  let footer: JSX.Element
  const cancel = <Button onClick={props.onClose}>Cancel</Button>
  switch (s.name) {
    case 'reviewing':
    case 'typing':
    case 'sending': {
      const word = needsWord(s.items)
      const typed = s.name === 'reviewing' ? '' : s.typed
      const why =
        s.name === 'sending'
          ? 'it is being sent'
          : s.items.length === 0 || !s.offer
            ? 'nothing waits for a confirmation'
            : canSend(s)
              ? undefined
              : `type ${confirmWord} first`
      body = (
        <>
          {st?.mode === 'observe' && <Banner tone="info">pco observes only: a confirmation starts publishing as well.</Banner>}
          <p>Confirmed, the daemon accepts exactly this at the next run, and refuses if it changed in the meantime.</p>
          <WaitingList items={s.items} />
          {s.offer && (
            <p className="offer muted">
              Offer <span className="mono">{s.offer}</span>
            </p>
          )}
          <UnaffectedList actions={st ? unaffected(st) : []} />
          {word && (
            <Field label={`Type ${confirmWord} to accept the removals`}>
              {(control) => (
                <input
                  {...control}
                  type="text"
                  autoComplete="off"
                  spellCheck={false}
                  value={typed}
                  disabled={s.name === 'sending'}
                  onChange={(e) => dispatch({ type: 'type', text: e.target.value })}
                />
              )}
            </Field>
          )}
          {s.name !== 'sending' && s.error && (
            <p className="error-line" role="alert">
              <FailIcon />
              <span>
                <Untrusted text={s.error} />
              </span>
            </p>
          )}
          {s.name === 'sending' && <Busy label="Sending the confirmation" />}
        </>
      )
      footer = (
        <>
          {cancel}
          <Button variant="primary" onClick={() => void send(s)} disabledReason={why}>
            {effect(s.items)}
          </Button>
        </>
      )
      break
    }
    case 'changed':
    case 'refused':
      body = (
        <>
          <div role="alert">
            <Banner tone="warn">
              <b>What waits changed while you were reading.</b> Nothing was confirmed: a list that was not read is never sent.
            </Banner>
          </div>
          {s.name === 'refused' ? (
            <p>
              The daemon says: <Untrusted text={s.message} />
            </p>
          ) : (
            <>
              <Lines lines={s.added} mark="added" />
              <Lines lines={s.removed} mark="removed" />
            </>
          )}
        </>
      )
      footer = (
        <>
          {cancel}
          <Button variant="primary" onClick={review}>
            Review the new list
          </Button>
        </>
      )
      break
    case 'done':
      body = (
        <>
          {s.accepted.length === 0 ? (
            <p>Nothing was confirmed.</p>
          ) : (
            <>
              <p>Confirmed for the next run:</p>
              <ul className="plain-list">
                {s.accepted.map((w) => (
                  <li key={`${w.kind}\u0000${w.subject}\u0000${w.detail}`}>
                    <Untrusted text={w.detail} />
                  </li>
                ))}
              </ul>
            </>
          )}
          {s.leftObserveOnly && <p>Publishing has started: the daemon changes Cloudflare from the next cycle.</p>}
        </>
      )
      footer = (
        <Button variant="primary" onClick={props.onClose}>
          Close
        </Button>
      )
      break
  }
  return (
    <Dialog open onClose={props.onClose} title="Confirm what waits" footer={footer}>
      <div className="confirm-state" data-state={s.name}>
        {body}
      </div>
    </Dialog>
  )
}
