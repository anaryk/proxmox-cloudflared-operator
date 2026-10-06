import './setup.css'

import { type ReactNode, useCallback, useEffect, useRef, useState } from 'react'

import { useApp } from '../../api/store'
import type { State } from '../../api/types.gen'
import { Head } from '../../app/Head'
import { navigate } from '../../app/router'
import { Badge } from '../../components/Badge'
import { Banner } from '../../components/Banner'
import { Button } from '../../components/Button'
import { Empty } from '../../components/Empty'
import { Skeleton } from '../../components/Skeleton'
import type { Tone } from '../../components/icons'
import { Overview } from '../Overview'
import { useAdmin } from '../routes/parts'
import { setDismissed, useSetupDismissed } from './dismissed'
import { StepInstall } from './StepInstall'
import { StepPublish } from './StepPublish'
import { StepReach } from './StepReach'
import { StepRoute } from './StepRoute'
import { type Mark, type Progress, progressOf, type StepId, type StepInfo } from './steps'
import { StepToken } from './StepToken'
import { StepZones } from './StepZones'

const markWords: Readonly<Record<Mark, [Tone, string]>> = {
  done: ['ok', 'done'],
  todo: ['info', 'to do'],
  wait: ['idle', 'waiting'],
  problem: ['fail', 'needs attention'],
  optional: ['idle', 'optional'],
  blocked: ['idle', 'not yet'],
}

// The number a step has in the setup; the install check is step 0, and shown
// only when it is needed.
const numbers: Readonly<Record<StepId, number>> = { install: 0, token: 1, zones: 2, reach: 3, route: 4, publish: 5 }

function Body({ id, st, p, go }: { id: StepId; st: State; p: Progress; go: (id: StepId) => void }) {
  switch (id) {
    case 'install':
      return <StepInstall st={st} />
    case 'token':
      return <StepToken st={st} p={p} go={go} />
    case 'zones':
      return <StepZones st={st} p={p} go={go} />
    case 'reach':
      return <StepReach st={st} p={p} go={go} />
    case 'route':
      return <StepRoute st={st} p={p} go={go} />
    case 'publish':
      return <StepPublish st={st} p={p} go={go} />
  }
}

function MarkBadge({ mark }: { mark: Mark }) {
  const [tone, word] = markWords[mark]
  return <Badge tone={tone}>{word}</Badge>
}

// The body of an open step. Whatever the admin does in it, typing or
// pressing a button, makes it the step the admin works in: it stays open when
// the state moves on to a later step, as it does when the token is stored or
// the route appears, so that its result can be read.
function StepBody({ id, onTouch, children }: { id: StepId; onTouch: (id: StepId) => void; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const touched = () => onTouch(id)
    const events = ['focusin', 'input', 'change']
    for (const e of events) el.addEventListener(e, touched)
    return () => {
      for (const e of events) el.removeEventListener(e, touched)
    }
  }, [id, onTouch])
  return (
    <div ref={ref} id={`step-${id}`} className="wizard-body" role="region" aria-labelledby={`step-${id}-head`}>
      {children}
    </div>
  )
}

// Steps lists the steps and opens one: the first that waits for the admin,
// until the admin opens another or starts working in the open one.
function Steps({ st, p }: { st: State; p: Progress }) {
  // undefined follows the first step to take; null is all closed
  const [chosen, setChosen] = useState<StepId | null>()
  const [focus, setFocus] = useState<StepId>()
  // a step that is gone, such as the install check once it is done, is not open
  const open = chosen === undefined || (chosen !== null && !p.steps.some((s) => s.id === chosen)) ? p.current : chosen
  const go = (id: StepId) => {
    setChosen(id)
    setFocus(id)
  }
  const touch = useCallback((id: StepId) => setChosen((c) => (c === undefined ? id : c)), [])
  useEffect(() => {
    if (focus) document.getElementById(`step-${focus}-head`)?.focus()
  }, [focus])

  return (
    <ol className="wizard" aria-label="Steps of the first-run setup">
      {p.steps.map((s: StepInfo) => {
        const isOpen = open === s.id
        return (
          <li key={s.id} className={`wizard-step wizard-${s.mark}${isOpen ? ' wizard-open' : ''}`}>
            <h2 className="wizard-head">
              <button
                type="button"
                id={`step-${s.id}-head`}
                aria-expanded={isOpen}
                aria-controls={`step-${s.id}`}
                onClick={() => {
                  setChosen(isOpen ? null : s.id)
                  setFocus(undefined)
                }}
              >
                <span className="wizard-num" aria-hidden="true">
                  {numbers[s.id]}
                </span>
                <span className="wizard-title">{s.title}</span>
                <MarkBadge mark={s.mark} />
              </button>
            </h2>
            {isOpen && (
              <StepBody id={s.id} onTouch={touch}>
                <Body id={s.id} st={st} p={p} go={go} />
              </StepBody>
            )}
          </li>
        )
      })}
    </ol>
  )
}

// A reader cannot do a step, and is told so, with how far the setup is.
function ForReaders({ p }: { p: Progress }) {
  return (
    <>
      <Empty title="An admin has to finish the setup">
        <p>The steps change what pco does, which only an admin may do.</p>
      </Empty>
      <ol className="wizard wizard-readonly" aria-label="Steps of the first-run setup">
        {p.steps.map((s) => (
          <li key={s.id} className={`wizard-step wizard-${s.mark}`}>
            <h2 className="wizard-head">
              <span className="wizard-num" aria-hidden="true">
                {numbers[s.id]}
              </span>
              <span className="wizard-title">{s.title}</span>
              <MarkBadge mark={s.mark} />
            </h2>
          </li>
        ))}
      </ol>
    </>
  )
}

// SetupPage is /setup, and the first page while there is no credential: the
// steps from a fresh install to the first published hostname.
export function SetupPage(): ReactNode {
  const st = useApp((s) => s.state)
  const admin = useAdmin()
  const dismissed = useSetupDismissed()
  const noCredential = st !== undefined && st.credentials.length === 0

  const skip = (
    <Button
      onClick={() => {
        setDismissed(true)
        navigate('/')
      }}
    >
      Skip for now
    </Button>
  )
  const head = <Head title="First-run setup" description="A token, the zones, and the first route." actions={noCredential && !dismissed ? skip : undefined} />

  if (!st) {
    return (
      <>
        {head}
        <Skeleton lines={5} label="Loading the state" />
      </>
    )
  }
  const p = progressOf(st)
  return (
    <>
      {head}
      {noCredential && dismissed && (
        <Banner
          tone="info"
          action={
            <Button small onClick={() => setDismissed(false)}>
              Show the setup first again
            </Button>
          }
        >
          You skipped the setup in this browser, so the Overview comes first here. The choice is kept per browser: another browser, or another admin, is still shown the
          setup first.
        </Banner>
      )}
      {admin ? <Steps st={st} p={p} /> : <ForReaders p={p} />}
    </>
  )
}

// Start is the first page. While there is no credential, and the setup was
// not skipped in this browser, it takes the admin to the setup, which stays
// where it is as the steps are done; the Overview otherwise.
export function Start(): ReactNode {
  const noCredential = useApp((s) => s.state !== undefined && s.state.credentials.length === 0)
  const dismissed = useSetupDismissed()
  const due = noCredential && !dismissed
  useEffect(() => {
    if (due) navigate('/setup', true)
  }, [due])
  return due ? <SetupPage /> : <Overview />
}
