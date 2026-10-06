import { useEffect, useState } from 'react'

import { ApiError } from '../../api/client'
import { explain } from '../../api/errors'
import { useApp } from '../../api/store'
import type { RouteView, SettingsView } from '../../api/types.gen'
import { useLocation } from '../../app/router'
import { Banner } from '../../components/Banner'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { Skeleton } from '../../components/Skeleton'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { getSettings, restartDaemon } from './api'
import { ExportImport } from './ExportImport'
import { type AllowRequest, SettingsForm } from './SettingsForm'
import { SettingsIssues } from './SettingsIssues'

export type AllowLink = { kind: 'none' } | { kind: 'wait' } | { kind: 'refused' } | { kind: 'accepted'; request: AllowRequest }

// allowLink reads /settings?addAllowHost=<pattern>&owner=<owner>, the link a
// route that waits for allowHosts offers. The pattern is taken only when it is
// the hostname of a rejected route of that owner in the state, never text from
// the address bar alone; anything else in the query is left unread. Without
// the state the link cannot be told yet.
export function allowLink(search: string, routes: readonly RouteView[] | undefined): AllowLink {
  const query = new URLSearchParams(search)
  const pattern = query.get('addAllowHost')
  if (pattern === null) return { kind: 'none' }
  if (routes === undefined) return { kind: 'wait' }
  const owner = query.get('owner') ?? ''
  const asked = routes.some((r) => r.state === 'rejected' && r.hostname === pattern && r.owner === owner)
  return asked ? { kind: 'accepted', request: { pattern, owner } } : { kind: 'refused' }
}

function RestartNotice({ fields, onDone }: { fields: readonly string[]; onDone: () => void }) {
  const toast = useToast()
  const [asking, setAsking] = useState(false)
  const [busy, setBusy] = useState(false)

  const restart = async () => {
    setBusy(true)
    try {
      await restartDaemon()
      toast('The daemon restarts: it finishes the running cycle and starts again.', 'ok')
      setAsking(false)
      onDone()
    } catch (e) {
      const said = e instanceof ApiError ? explain(e) : { text: String(e), quoted: false }
      toast(said.quoted ? <Untrusted text={said.text} /> : said.text, 'fail')
      setAsking(false)
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <Banner tone="info" action={<Button small onClick={() => setAsking(true)}>Restart pco…</Button>}>
        <b>{fields.join(', ')}</b> {fields.length === 1 ? 'is' : 'are'} read only when pco starts: {fields.length === 1 ? 'it takes' : 'they take'} effect after{' '}
        <span className="mono">systemctl restart pco</span>.
      </Banner>
      <Dialog
        open={asking}
        onClose={() => setAsking(false)}
        title="Restart the daemon?"
        footer={
          <>
            <Button onClick={() => setAsking(false)}>Cancel</Button>
            <Button variant="primary" disabled={busy} onClick={() => void restart()}>
              {busy ? 'Restarting…' : 'Restart'}
            </Button>
          </>
        }
      >
        <p>
          The daemon finishes the running cycle and stops, and systemd starts it again. The connectors keep running, and so do the routes they serve. Nothing
          changes at Cloudflare while it is away.
        </p>
      </Dialog>
    </>
  )
}

// SettingsPage is the settings of the daemon: what the daemon says is wrong
// with them, every one of them in a form, and a file to take them elsewhere.
export function SettingsPage() {
  const issues = useApp((s) => s.state?.issues)
  const routes = useApp((s) => s.state?.routes)
  const role = useApp((s) => s.session?.role)
  const node = useApp((s) => s.session?.node ?? '')
  const location = useLocation()
  const [view, setView] = useState<SettingsView>()
  const [failed, setFailed] = useState<ApiError>()
  const [restart, setRestart] = useState<string[]>([])
  const canWrite = role === 'admin'

  // Asking again is a new round.
  const [round, setRound] = useState(0)

  useEffect(() => {
    let current = true
    getSettings().then(
      (v) => {
        if (!current) return
        setView(v)
        setFailed(undefined)
      },
      (e: unknown) => {
        if (current) setFailed(e instanceof ApiError ? e : new ApiError(0, { error: String(e) }))
      },
    )
    return () => {
      current = false
    }
  }, [round])

  const link = allowLink(new URL(location, 'https://page.invalid').search, routes)
  const allow = canWrite && link.kind === 'accepted' ? link.request : undefined

  const imported = (needed: string[]) => {
    setRestart(needed)
    setView(undefined)
    setRound((r) => r + 1)
  }

  let body
  if (failed) {
    const said = explain(failed)
    body = (
      <Banner
        tone="fail"
        action={
          <Button
            small
            onClick={() => {
              setFailed(undefined)
              setRound((r) => r + 1)
            }}
          >
            Try again
          </Button>
        }
      >
        {said.quoted ? <Untrusted text={said.text} /> : said.text}
      </Banner>
    )
  } else if (!view || (canWrite && link.kind === 'wait')) {
    body = <Skeleton lines={6} label="Loading the settings" />
  } else {
    body = (
      <>
        {canWrite && link.kind === 'refused' && <Banner tone="warn">This link names no rejected route, so nothing was added to the allowed hostnames.</Banner>}
        {allow && (
          <Banner tone="info">
            <Untrusted text={allow.pattern} hostname /> is in the allowed hostnames below, for the route of <Untrusted text={allow.owner} />. Nothing is saved until
            you press Save.
          </Banner>
        )}
        <SettingsForm view={view} canWrite={canWrite} allow={allow} onSaved={(saved) => setRestart(saved.restartNeeded)} />
      </>
    )
  }

  return (
    <>
      <SettingsIssues issues={issues ?? []} notes={view?.notes} />
      {restart.length > 0 && <RestartNotice fields={restart} onDone={() => setRestart([])} />}
      {body}
      <ExportImport node={node} canWrite={canWrite} onImported={imported} />
    </>
  )
}
