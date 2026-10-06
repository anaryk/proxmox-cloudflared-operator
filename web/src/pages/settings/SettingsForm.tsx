import './settings.css'

import { type ChangeEvent, type FormEvent, type ReactNode, useEffect, useId, useMemo, useRef, useState } from 'react'

import { ApiError } from '../../api/client'
import { explain } from '../../api/errors'
import { useApp } from '../../api/store'
import type { Limit, SettingsView } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { Field } from '../../components/Field'
import { FailIcon } from '../../components/icons'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { getSettings, putSettings, type SavedSettings } from './api'
import { Changes, FirstAllowed } from './Changes'
import { diffSettings, firstAllowed, optInWords } from './diff'
import { addAllowHost, type Draft, draftIssues, draftOf, mergeDraft, settingsOf } from './draft'
import { type FieldIssue, validateSettings } from './validate'

// A pattern in allowHosts that a route waits for: the hostname the route
// asks for, and who asks.
export interface AllowRequest {
  pattern: string
  owner: string
}

type TextName = { [K in keyof Draft]: Draft[K] extends string ? K : never }[keyof Draft]

// The defaults of a fresh install (store.DefaultSettings), for the hints.
const defaults = {
  gateTag: 'cf-tunnel',
  pollInterval: '10s',
  grace: '1m0s',
  reverifyInterval: '1m0s',
  maxHostnamesPerGuest: '32',
  cloudflareBudget: '1000',
}

const admissions: [string, string][] = [
  ['tag', 'A guest that carries the gate tag is published.'],
  [
    'approve',
    'A guest that carries the gate tag is published once an admin approved it. Use this when anyone but the admins holds VM.Clone on a tagged guest or template: Proxmox copies the tag to the clone.',
  ],
]

const patterns =
  'A pattern is *, a hostname, or *. followed by labels: *.example.com covers every name below example.com at any depth, and not example.com itself.'

// The controls of the form: a setting that has none is shown below the form.
const controlNames = new Set([
  'observeOnly',
  'admission',
  'allowHosts',
  'denyHosts',
  'identityMinimum',
  'trustedCIDRs',
  'manualCIDRs',
  'pollInterval',
  'grace',
  'reverifyInterval',
  'maxHostnamesPerGuest',
  'cloudflareBudget',
  'zonePins',
  'gateTag',
])

// The name of the control a field of the daemon's answer belongs to:
// "denyHosts[2]" is the list denyHosts.
const controlOf = (field: string) => field.replace(/[[.].*$/, '')

function rangeText(l: Limit | undefined): string {
  if (!l) return ''
  const [min, max] = [l.min !== undefined, l.max !== undefined]
  if (min && max) return `From ${String(l.min)} to ${String(l.max)}.`
  if (min) return `At least ${String(l.min)}.`
  if (max) return `At most ${String(l.max)}.`
  return ''
}

// What the two choices do with a setting both sides changed.
const bothWords = (n: number) => {
  const it = n === 1 ? 'it' : 'them'
  return `Keeping your edits keeps your value of ${it}; using the saved settings drops ${it}.`
}

function Section({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <section className="card settings-section" aria-labelledby={id}>
      <div className="card-head">
        <h2 id={id}>{title}</h2>
      </div>
      <div className="card-body">{children}</div>
    </section>
  )
}

// Check is a switch with its hint, tied to it.
function Check({ label, hint, checked, onChange }: { label: string; hint: ReactNode; checked: boolean; onChange: (on: boolean) => void }) {
  const id = useId()
  return (
    <div className="field">
      <label className="check" htmlFor={id}>
        <input id={id} type="checkbox" checked={checked} aria-describedby={`${id}hint`} onChange={(e) => onChange(e.target.checked)} />
        {label}
      </label>
      <p id={`${id}hint`} className="field-hint">
        {hint}
      </p>
    </div>
  )
}

function ErrorLine({ children }: { children: ReactNode }) {
  if (children === undefined) return null
  return (
    <p className="field-error" role="alert">
      <FailIcon />
      <span>{children}</span>
    </p>
  )
}

export interface SettingsFormProps {
  view: SettingsView
  // Only an admin may change the settings.
  canWrite: boolean
  allow?: AllowRequest
  onSaved: (saved: SavedSettings) => void
  // Told whether the form holds changes that are not saved.
  onDirty?: (dirty: boolean) => void
}

// SettingsForm is every setting, in sections, with the words of the
// documentation. The page checks what it can as it is typed; the daemon
// checks every save, and its answer wins.
export function SettingsForm({ view, canWrite, allow, onSaved, onDirty }: SettingsFormProps) {
  const toast = useToast()
  const routes = useApp((s) => s.state?.routes)
  const [base, setBase] = useState(view)
  const allowKey = allow ? `${allow.owner}\n${allow.pattern}` : ''
  const [draft, setDraft] = useState(() => (allow ? addAllowHost(draftOf(view.settings), allow.pattern) : draftOf(view.settings)))
  const [seenAllow, setSeenAllow] = useState(allowKey)
  if (seenAllow !== allowKey) {
    setSeenAllow(allowKey)
    if (allow) setDraft((d) => addAllowHost(d, allow.pattern))
  }
  const [attempted, setAttempted] = useState(false)
  const [server, setServer] = useState<FieldIssue>()
  const [review, setReview] = useState(false)
  const [saving, setSaving] = useState(false)
  const [conflict, setConflict] = useState<{ message: string; fresh: SettingsView }>()
  const [focusFirst, setFocusFirst] = useState(0)
  const form = useRef<HTMLFormElement>(null)
  const allowField = useRef<HTMLTextAreaElement>(null)

  const clean = useMemo(() => draftOf(base.settings), [base.settings])
  const settings = useMemo(() => settingsOf(draft, base.settings), [draft, base.settings])
  const issues = useMemo(() => [...validateSettings(settings, base.limits, base.settings), ...draftIssues(draft)], [settings, base, draft])
  const changes = useMemo(() => diffSettings(base.settings, settings), [base.settings, settings])
  const { limits, readAtStart } = base

  useEffect(() => {
    if (allowKey) allowField.current?.focus()
  }, [allowKey])

  const dirty = changes.length > 0
  useEffect(() => {
    onDirty?.(dirty)
  }, [dirty, onDirty])

  useEffect(() => {
    if (focusFirst > 0) form.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus()
  }, [focusFirst])

  const edit = (patch: Partial<Draft>) => {
    setDraft((d) => ({ ...d, ...patch }))
    setServer(undefined)
  }
  const bind = (name: TextName) => ({
    name,
    value: draft[name],
    onChange: (e: ChangeEvent<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>) => edit({ [name]: e.target.value } as Partial<Draft>),
  })

  // What is wrong with a control: the daemon's word where it refused a save,
  // else what the page found, in a control that was edited. The error shows
  // as it is typed, not when the control is left: a message that appears when
  // the pointer goes for the Save button would move the button away.
  const wrong = (name: keyof Draft): ReactNode => {
    if (server && controlOf(server.field) === name) return <Untrusted text={server.message} />
    if (draft[name] === clean[name]) return undefined
    const mine = issues.filter((i) => controlOf(i.field) === name)
    if (mine.length === 0) return undefined
    return mine.map((i, at) => (
      <span key={at} className="line">
        <Untrusted text={i.message} />
      </span>
    ))
  }
  // The badge of a setting the daemon reads only at its start: the daemon
  // says which, the form has no list of its own.
  const start = (name: string): ReactNode =>
    readAtStart.includes(name) && (
      <>
        <Badge tone="info">read at start</Badge>{' '}
      </>
    )
  const hint = (name: string, text: ReactNode): ReactNode => (
    <>
      {start(name)}
      {text}
    </>
  )

  const discard = () => {
    setDraft(draftOf(base.settings))
    setServer(undefined)
    setAttempted(false)
  }

  const submit = (e: FormEvent) => {
    e.preventDefault()
    if (!canWrite || changes.length === 0) return
    setAttempted(true)
    if (issues.length > 0) setFocusFirst((n) => n + 1)
    else setReview(true)
  }

  const save = async () => {
    setSaving(true)
    try {
      const saved = await putSettings(base.rev, settings)
      setBase(saved)
      setDraft(draftOf(saved.settings))
      setAttempted(false)
      setReview(false)
      toast(`The settings are saved; they are at revision ${saved.rev}.`, 'ok')
      onSaved(saved)
    } catch (e) {
      setReview(false)
      if (e instanceof ApiError && e.code === 'invalid') {
        setServer({ field: e.field ?? '', message: e.message })
        setFocusFirst((n) => n + 1)
      } else if (e instanceof ApiError && e.code === 'refused') {
        await showConflict(e)
      } else {
        const said = e instanceof ApiError ? explain(e) : { text: String(e), quoted: false }
        toast(said.quoted ? <Untrusted text={said.text} /> : said.text, 'fail')
      }
    } finally {
      setSaving(false)
    }
  }

  // Someone saved in between: the settings as they are now, and both sets of
  // changes, each against what the page was opened on.
  const showConflict = async (refused: ApiError) => {
    try {
      setConflict({ message: refused.message, fresh: await getSettings() })
    } catch (e) {
      const said = e instanceof ApiError ? explain(e) : { text: String(e), quoted: false }
      toast(said.quoted ? <Untrusted text={said.text} /> : said.text, 'fail')
    }
  }

  const keepMine = () => {
    if (!conflict) return
    setDraft((d) => mergeDraft(d, draftOf(base.settings), draftOf(conflict.fresh.settings)))
    setBase(conflict.fresh)
    setConflict(undefined)
  }

  const useSaved = () => {
    if (!conflict) return
    setBase(conflict.fresh)
    setDraft(draftOf(conflict.fresh.settings))
    setAttempted(false)
    setConflict(undefined)
  }

  const optIns = allow && changes.find((c) => c.field === 'allowHosts')?.added?.includes(allow.pattern) ? [allow] : []
  const stopped = firstAllowed(base.settings, settings, routes ?? [])
  const pending = changes.map((c) => c.field).filter((f) => readAtStart.includes(f))
  const theirs = conflict ? diffSettings(base.settings, conflict.fresh.settings) : []
  const both = new Set(theirs.map((c) => c.field).filter((f) => changes.some((c) => c.field === f)))
  const leaving = settings.observeOnly && !base.settings.observeOnly
  const general = server && !controlNames.has(controlOf(server.field)) ? server : undefined
  const id = useId()

  return (
    <>
      <form ref={form} className="settings-form" onSubmit={submit} noValidate aria-label="Settings">
        <fieldset className="settings-fieldset" disabled={!canWrite}>
          <Section id={`${id}publishing`} title="Publishing">
            {base.settings.observeOnly ? (
              <p>
                {start('observeOnly')}pco only observes: it reads the guests and plans, and changes nothing at Cloudflare. <Link to="/routes/plan">Review the plan and start publishing</Link>.
              </p>
            ) : (
              <>
                <p>
                  {start('observeOnly')}pco publishes what it plans.
                </p>
                <Button aria-pressed={draft.observeOnly} onClick={() => edit({ observeOnly: !draft.observeOnly })}>
                  Return to observe-only
                </Button>
                {draft.observeOnly && <p className="field-hint">After this save pco only observes: nothing changes at Cloudflare until pco apply.</p>}
              </>
            )}
            <ErrorLine>{wrong('observeOnly')}</ErrorLine>
          </Section>

          <Section id={`${id}admission`} title="Admission">
            <fieldset className="radios">
              <legend>
                {start('admission')}Which guests are published
              </legend>
              {admissions.map(([value, words]) => (
                <div key={value} className="radio">
                  <input
                    id={`${id}${value}`}
                    type="radio"
                    name="admission"
                    value={value}
                    checked={draft.admission === value}
                    aria-describedby={`${id}${value}hint`}
                    onChange={() => edit({ admission: value })}
                  />
                  <label htmlFor={`${id}${value}`}>{value}</label>
                  <p id={`${id}${value}hint`} className="field-hint">
                    {words}
                  </p>
                </div>
              ))}
            </fieldset>
            <ErrorLine>{wrong('admission')}</ErrorLine>
          </Section>

          <Section id={`${id}policy`} title="Hostname policy">
            <Field
              label="Allowed hostnames"
              hint={hint(
                'allowHosts',
                <>
                  Patterns of the hostnames that may be published, one a line. With none, every hostname may be published but the apex of a zone and a wildcard,
                  which a guest publishes only when a pattern names it; with one, only the hostnames a pattern matches. {patterns}
                </>,
              )}
              error={wrong('allowHosts')}
            >
              {(c) => <textarea {...c} {...bind('allowHosts')} ref={allowField} rows={4} spellCheck={false} autoComplete="off" autoCapitalize="off" />}
            </Field>
            <Field
              label="Denied hostnames"
              hint={hint('denyHosts', 'Patterns of the hostnames that may not be published, one a line. A deny rule wins over an allow rule.')}
              error={wrong('denyHosts')}
            >
              {(c) => <textarea {...c} {...bind('denyHosts')} rows={4} spellCheck={false} autoComplete="off" autoCapitalize="off" />}
            </Field>
          </Section>

          <Section id={`${id}identity`} title="Identity">
            <Field
              label="Lowest identity level served"
              hint={hint(
                'identityMinimum',
                'The lowest level at which the address of a guest is served: port, filtered or observed. port is the strictest; observed also serves guests of other nodes and trusted static addresses, where only ARP is proven. Know what a lower level proves before you choose it. Default port.',
              )}
              error={wrong('identityMinimum')}
            >
              {(c) => (
                <select {...c} {...bind('identityMinimum')}>
                  <option value="port">port</option>
                  <option value="filtered">filtered</option>
                  <option value="observed">observed</option>
                </select>
              )}
            </Field>
            <Check
              label="Trust static addresses behind a router"
              checked={draft.trustStatic}
              onChange={(on) => edit({ trustStatic: on })}
              hint={hint('trustStatic', 'Static addresses of guests that a gateway routes to are proven at observed only, in the prefixes below.')}
            />
            <Field
              label="Prefixes of trusted static addresses"
              hint={hint('trustedCIDRs', 'The IPv4 prefixes in which static addresses are trusted, one a line, such as 10.0.20.0/24.')}
              error={wrong('trustedCIDRs')}
            >
              {(c) => <textarea {...c} {...bind('trustedCIDRs')} rows={3} spellCheck={false} autoComplete="off" />}
            </Field>
          </Section>

          <Section id={`${id}manual`} title="Manual routes">
            <Field
              label="Prefixes of manual route addresses"
              hint={hint(
                'manualCIDRs',
                'The IPv4 prefixes the address of a manual route must lie in, one a line. It is checked when a route is made or changed, so a change here needs no restart. With none, no route to an address can be made. The prefixes of trusted static addresses have no say in it.',
              )}
              error={wrong('manualCIDRs')}
            >
              {(c) => <textarea {...c} {...bind('manualCIDRs')} rows={3} spellCheck={false} autoComplete="off" />}
            </Field>
          </Section>

          <Section id={`${id}timing`} title="Timing">
            <div className="settings-row">
              <Field
                label="Poll interval"
                hint={hint('pollInterval', `The time between two cycles. ${rangeText(limits.pollInterval)} Default ${defaults.pollInterval}.`)}
                error={wrong('pollInterval')}
              >
                {(c) => <input {...c} {...bind('pollInterval')} type="text" spellCheck={false} autoComplete="off" />}
              </Field>
              <Field
                label="Grace"
                hint={hint('grace', `How long a removal waits before it is made. ${rangeText(limits.grace)} Default ${defaults.grace}.`)}
                error={wrong('grace')}
              >
                {(c) => <input {...c} {...bind('grace')} type="text" spellCheck={false} autoComplete="off" />}
              </Field>
              <Field
                label="Proof of identity stands for"
                hint={hint(
                  'reverifyInterval',
                  `How long a proof of identity that the watch of the network vouches for stands before the address is checked on the wire again. ${rangeText(limits.reverifyInterval)} Default ${defaults.reverifyInterval}.`,
                )}
                error={wrong('reverifyInterval')}
              >
                {(c) => <input {...c} {...bind('reverifyInterval')} type="text" spellCheck={false} autoComplete="off" />}
              </Field>
            </div>
          </Section>

          <Section id={`${id}limits`} title="Limits">
            <div className="settings-row">
              <Field
                label="Hostnames per guest"
                hint={hint(
                  'maxHostnamesPerGuest',
                  `How many hostnames the Notes of one guest may name. A guest that names more publishes none of them. ${rangeText(limits.maxHostnamesPerGuest)} Default ${defaults.maxHostnamesPerGuest}.`,
                )}
                error={wrong('maxHostnamesPerGuest')}
              >
                {(c) => <input {...c} {...bind('maxHostnamesPerGuest')} type="text" inputMode="numeric" autoComplete="off" />}
              </Field>
              <Field
                label="Cloudflare budget"
                hint={hint(
                  'cloudflareBudget',
                  `The requests in 5 minutes that the clients of one credential spend at most, of the 1200 Cloudflare allows a user, so that the dashboard and other tools keep the rest. ${rangeText(limits.cloudflareBudget)} Default ${defaults.cloudflareBudget}.`,
                )}
                error={wrong('cloudflareBudget')}
              >
                {(c) => <input {...c} {...bind('cloudflareBudget')} type="text" inputMode="numeric" autoComplete="off" />}
              </Field>
            </div>
          </Section>

          <Section id={`${id}zones`} title="Zones">
            <Field
              label="Zone pins"
              hint={hint('zonePins', 'A zone and the id of the credential that serves it, one pair a line: example.com a1b2c3d4.')}
              error={wrong('zonePins')}
            >
              {(c) => <textarea {...c} {...bind('zonePins')} rows={3} spellCheck={false} autoComplete="off" autoCapitalize="off" />}
            </Field>
          </Section>

          <Section id={`${id}gate`} title="Gate tag">
            <Field
              label="Tag"
              hint={hint(
                'gateTag',
                <>
                  The tag that makes a guest a candidate: lower-case letters, digits and the characters _ - + ., at most 64 characters. After you change it, run{' '}
                  <code className="mono">pco setup --repair</code> on the node. Default {defaults.gateTag}.
                </>,
              )}
              error={wrong('gateTag')}
            >
              {(c) => <input {...c} {...bind('gateTag')} type="text" spellCheck={false} autoComplete="off" autoCapitalize="off" />}
            </Field>
          </Section>
        </fieldset>

        {canWrite ? (
          <div className="settings-actions">
            <Button type="submit" variant="primary" disabledReason={changes.length === 0 ? 'No changes to save' : undefined}>
              Save
            </Button>
            <Button onClick={discard} disabled={changes.length === 0 && server === undefined}>
              Discard changes
            </Button>
            {attempted && issues.length > 0 && (
              <p className="field-error" role="alert">
                <FailIcon />
                <span>Fix the settings marked above before saving.</span>
              </p>
            )}
            {general && (
              <p className="field-error" role="alert">
                <FailIcon />
                <Untrusted text={general.message} />
              </p>
            )}
          </div>
        ) : (
          <p className="muted">Only admins can change the settings. You can read them here and export them below.</p>
        )}
      </form>

      <Dialog
        open={review}
        onClose={() => setReview(false)}
        title="Save these changes?"
        footer={
          <>
            <Button onClick={() => setReview(false)}>Cancel</Button>
            <Button variant="primary" disabled={saving} onClick={() => void save()}>
              {saving ? 'Saving…' : 'Save settings'}
            </Button>
          </>
        }
      >
        <Changes changes={changes} label="Changes to the settings" />
        {optIns.map((o) => (
          <p key={o.pattern} className="settings-optin">
            <Untrusted text={`${o.pattern} is asked for by ${o.owner}. ${optInWords(o.pattern, o.owner)}`} />
          </p>
        ))}
        {stopped && <FirstAllowed stopped={stopped} />}
        {pending.length > 0 && (
          <p className="muted">
            {pending.join(', ')} {pending.length === 1 ? 'is' : 'are'} read only when pco starts: {pending.length === 1 ? 'it takes' : 'they take'} effect after
            systemctl restart pco.
          </p>
        )}
        {leaving && <p className="muted">After this save pco only observes, and changes nothing at Cloudflare until pco apply.</p>}
      </Dialog>

      <Dialog
        open={conflict !== undefined}
        onClose={() => setConflict(undefined)}
        title="The settings changed while you edited"
        footer={
          <>
            <Button onClick={useSaved}>Use the saved settings</Button>
            <Button variant="primary" onClick={keepMine}>
              Keep my edits
            </Button>
          </>
        }
      >
        {conflict && (
          <>
            <p>
              <Untrusted text={conflict.message} />
            </p>
            <h3>Saved by someone else, at revision {conflict.fresh.rev}</h3>
            <Changes changes={theirs} label="Saved since the page was opened" both={both} />
            <h3>Your edits</h3>
            <Changes changes={changes} label="Your edits" both={both} />
            {both.size > 0 && (
              <p>
                Changed on both sides: <span className="mono">{[...both].join(', ')}</span>. {bothWords(both.size)}
              </p>
            )}
            <p className="muted">Keeping your edits puts them on the saved settings: a setting you did not touch is the saved one.</p>
          </>
        )}
      </Dialog>
    </>
  )
}
