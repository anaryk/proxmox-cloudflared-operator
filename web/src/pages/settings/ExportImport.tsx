import { type ChangeEvent, type ReactNode, useRef, useState } from 'react'

import { ApiError } from '../../api/client'
import { explain } from '../../api/errors'
import type { ManualRouteView, SettingsView } from '../../api/types.gen'
import { Button } from '../../components/Button'
import { Busy } from '../../components/Busy'
import { Dialog } from '../../components/Dialog'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { createRoute, deleteRoute, getRoutes, getSettings, putSettings, updateRoute } from './api'
import { Changes } from './Changes'
import { diffSettings, planRoutes, routeText } from './diff'
import { type ImportFile, type ImportIssue, checkImport } from './manual'
import { importSteps, type Outcome, runSteps } from './importRun'

// The largest file the web process takes for a save of the settings.
const maxFile = 1 << 20

const pad = (n: number) => String(n).padStart(2, '0')

// exportName is pco-settings-<node>-<yyyymmdd>.json, by the date of the
// browser.
export function exportName(node: string, at: Date): string {
  const name = node.replace(/[^A-Za-z0-9._-]/g, '-') || 'node'
  return `pco-settings-${name}-${at.getFullYear()}${pad(at.getMonth() + 1)}${pad(at.getDate())}.json`
}

// exportText is the file: the revision the settings were read at, the
// settings and the manual routes, as the daemon says them. No secret and no
// approval is in either.
export function exportText(view: SettingsView, routes: readonly ManualRouteView[]): string {
  return `${JSON.stringify({ rev: view.rev, settings: view.settings, manualRoutes: routes }, null, 2)}\n`
}

function download(name: string, text: string): void {
  const url = URL.createObjectURL(new Blob([text], { type: 'application/json' }))
  const link = document.createElement('a')
  link.href = url
  link.download = name
  document.body.append(link)
  link.click()
  link.remove()
  setTimeout(() => URL.revokeObjectURL(url), 0)
}

type Mode = 'merge' | 'replace'

type Shown =
  | { kind: 'refused'; issues: ImportIssue[] }
  | { kind: 'review'; file: ImportFile; view: SettingsView; routes: ManualRouteView[] }
  | { kind: 'done'; outcome: Outcome; restart: string[] }

const said = (e: unknown): { text: string; quoted: boolean } => (e instanceof ApiError ? explain(e) : { text: String(e), quoted: false })

function Quoted({ text, quoted }: { text: string; quoted: boolean }) {
  return quoted ? <Untrusted text={text} /> : <>{text}</>
}

function Report({ outcome }: { outcome: Outcome }) {
  const { saved, failed, notSaved } = outcome
  return (
    <>
      {failed ? (
        <p role="alert">
          Stopped at <b>{failed.label}</b>: <Quoted text={failed.text} quoted={failed.quoted} />
          {failed.unknown && ' Whether it was saved is not known.'}
        </p>
      ) : (
        <p>Everything of the file is saved.</p>
      )}
      {saved.length > 0 && (
        <>
          <h3>Saved</h3>
          <ul>
            {saved.map((s) => (
              <li key={s}>
                <Untrusted text={s} />
              </li>
            ))}
          </ul>
        </>
      )}
      {notSaved.length > 0 && (
        <>
          <h3>Not saved</h3>
          <ul>
            {notSaved.map((s) => (
              <li key={s}>
                <Untrusted text={s} />
              </li>
            ))}
          </ul>
        </>
      )}
    </>
  )
}

function RouteList({ title, items }: { title: string; items: ReactNode[] }) {
  if (items.length === 0) return null
  return (
    <>
      <h3>
        {title} ({items.length})
      </h3>
      <ul>
        {items.map((item, at) => (
          <li key={at}>{item}</li>
        ))}
      </ul>
    </>
  )
}

export interface ExportImportProps {
  node: string
  canWrite: boolean
  // The settings and routes changed: what to show again, and the settings
  // that take effect only after a restart.
  onImported: (restartNeeded: string[]) => void
  now?: () => Date
}

// ExportImport downloads the settings and the manual routes as one file, and
// reads such a file back: all of it is checked first, nothing is written
// until the diff is confirmed, and the writes stop at the first refusal.
export function ExportImport({ node, canWrite, onImported, now = () => new Date() }: ExportImportProps) {
  const toast = useToast()
  const picker = useRef<HTMLInputElement>(null)
  const [busy, setBusy] = useState<'export' | 'read' | 'import'>()
  const [shown, setShown] = useState<Shown>()
  const [mode, setMode] = useState<Mode>('merge')

  const fail = (e: unknown) => {
    const s = said(e)
    toast(<Quoted text={s.text} quoted={s.quoted} />, 'fail')
  }

  const exportFile = async () => {
    setBusy('export')
    try {
      const [view, routes] = await Promise.all([getSettings(), getRoutes()])
      download(exportName(node, now()), exportText(view, routes))
    } catch (e) {
      fail(e)
    } finally {
      setBusy(undefined)
    }
  }

  const read = async (e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    e.target.value = ''
    if (!file) return
    if (file.size > maxFile) {
      setShown({ kind: 'refused', issues: [{ where: 'file', message: `the file is larger than ${maxFile} bytes` }] })
      return
    }
    setBusy('read')
    try {
      const text = await file.text()
      const [view, routes] = await Promise.all([getSettings(), getRoutes()])
      const checked = checkImport(text, { current: view.settings, limits: view.limits })
      setMode('merge')
      setShown(checked.file ? { kind: 'review', file: checked.file, view, routes } : { kind: 'refused', issues: checked.issues })
    } catch (err) {
      fail(err)
    } finally {
      setBusy(undefined)
    }
  }

  const run = async (r: Extract<Shown, { kind: 'review' }>) => {
    setBusy('import')
    let restart: string[] = []
    const changes = diffSettings(r.view.settings, r.file.settings)
    const plan = planRoutes(r.routes, r.file.routes ?? [], mode)
    const steps = importSteps(
      { rev: r.view.rev, settings: changes.length > 0 ? r.file.settings : undefined, plan },
      {
        putSettings: async (rev, s) => {
          const saved = await putSettings(rev, s)
          restart = saved.restartNeeded
          return saved
        },
        createRoute,
        updateRoute,
        deleteRoute,
      },
    )
    const outcome = await runSteps(steps)
    setBusy(undefined)
    setShown({ kind: 'done', outcome, restart })
    onImported(restart)
  }

  const close = () => setShown(undefined)

  let review: ReactNode = null
  if (shown?.kind === 'review') {
    const { file, view, routes } = shown
    const changes = diffSettings(view.settings, file.settings)
    const plan = planRoutes(routes, file.routes ?? [], mode)
    const nothing = changes.length === 0 && plan.add.length + plan.update.length + plan.remove.length === 0
    review = (
      <Dialog
        open
        onClose={close}
        title="Import this file?"
        footer={
          <>
            <Button onClick={close}>{nothing ? 'Close' : 'Cancel'}</Button>
            {!nothing && (
              <Button variant="primary" disabled={busy === 'import'} onClick={() => void run(shown)}>
                {busy === 'import' ? 'Importing…' : 'Import'}
              </Button>
            )}
          </>
        }
      >
        <p>The file passed every check. Nothing has been written yet.</p>
        <h3>Settings</h3>
        {changes.length > 0 ? <Changes changes={changes} label="Changes to the settings" /> : <p className="muted">The settings of the file are the saved ones.</p>}
        {file.routes ? (
          <>
            <h3>Manual routes</h3>
            <fieldset className="radios">
              <legend>What to do with the routes that are saved</legend>
              {(
                [
                  ['merge', 'Merge', 'Add the routes of the file and update those with the same id. Delete none.'],
                  ['replace', 'Replace', 'Also delete the routes the file does not have.'],
                ] as const
              ).map(([value, label, words]) => (
                <div key={value} className="radio">
                  <input id={`import-${value}`} type="radio" name="import-mode" value={value} checked={mode === value} aria-describedby={`import-${value}-hint`} onChange={() => setMode(value)} />
                  <label htmlFor={`import-${value}`}>{label}</label>
                  <p id={`import-${value}-hint`} className="field-hint">
                    {words}
                  </p>
                </div>
              ))}
            </fieldset>
            <RouteList title="Added" items={plan.add.map((r) => <Untrusted key={r.id} text={`${r.id}: ${routeText(r)}`} />)} />
            <RouteList
              title="Updated"
              items={plan.update.map((u) => <Untrusted key={u.after.id} text={`${u.after.id}: ${u.changes.join('; ')}`} />)}
            />
            <RouteList title="Deleted" items={plan.remove.map((r) => <Untrusted key={r.id} text={`${r.id}: ${routeText(r)}`} />)} />
            {plan.same.length > 0 && <p className="muted">{plan.same.length === 1 ? '1 route is the same as saved.' : `${plan.same.length} routes are the same as saved.`}</p>}
          </>
        ) : (
          <p className="muted">The file has no manual routes; those saved stay as they are.</p>
        )}
        {nothing && <p>There is nothing to import: the file matches what is saved.</p>}
      </Dialog>
    )
  }

  return (
    <section className="card settings-section" aria-labelledby="settings-transfer">
      <div className="card-head">
        <h2 id="settings-transfer">Export and import</h2>
      </div>
      <div className="card-body">
        <p>
          The file holds the settings and the manual routes, and no secret and no approval. Import checks all of it first and writes nothing until you confirm the
          changes.
        </p>
        <div className="settings-actions">
          <Button disabled={busy === 'export'} onClick={() => void exportFile()}>
            Export
          </Button>
          <Button
            disabled={busy === 'read'}
            disabledReason={canWrite ? undefined : 'Only admins can import'}
            onClick={() => picker.current?.click()}
          >
            Import…
          </Button>
          <input ref={picker} type="file" accept="application/json,.json" hidden aria-label="Settings file" onChange={(e) => void read(e)} />
          {(busy === 'export' || busy === 'read') && <Busy label={busy === 'export' ? 'Reading the settings' : 'Checking the file'} />}
        </div>
      </div>

      {review}

      {shown?.kind === 'refused' && (
        <Dialog open onClose={close} title="This file cannot be imported" footer={<Button onClick={close}>Close</Button>}>
          <p>Nothing was written. Fix these in the file, and import it again:</p>
          <ul>
            {shown.issues.map((i, at) => (
              <li key={at}>
                <b>
                  <Untrusted text={i.where} />
                </b>
                : <Untrusted text={i.message} />
              </li>
            ))}
          </ul>
        </Dialog>
      )}

      {shown?.kind === 'done' && (
        <Dialog open onClose={close} title="Import" footer={<Button onClick={close}>Close</Button>}>
          <Report outcome={shown.outcome} />
          {shown.restart.length > 0 && <p>{shown.restart.join(', ')} take effect after systemctl restart pco.</p>}
        </Dialog>
      )}
    </section>
  )
}
