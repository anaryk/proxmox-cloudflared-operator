import { api } from '../../api/client'
import { useApp } from '../../api/store'
import type { Step } from '../../api/types.gen'
import { Badge } from '../../components/Badge'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Stepper, type Step as StepperStep } from '../../components/Stepper'
import { Time } from '../../components/Time'
import { Untrusted } from '../../components/Untrusted'
import { changedSince, type DiagnosisStore, diagnoses, useDiagnoses } from './diagnosis.ts'
import { ErrorText } from './parts'

export const skippedWord = 'skipped: an earlier step failed'

// stepsOf are the steps of the daemon in the stepper's terms. A step after
// the first failure is skipped: the daemon says so with skipped, not with a
// level, which stays warn for older clients.
export function stepsOf(steps: readonly Step[]): StepperStep[] {
  return steps.map((s) => ({
    key: s.name,
    name: <Untrusted text={s.name} />,
    level: s.skipped ? 'skipped' : s.level,
    word: s.skipped ? skippedWord : undefined,
    detail: s.skipped || !s.detail ? undefined : <Untrusted text={s.detail} />,
  }))
}

// Diagnosis runs pco diagnose for a hostname and shows its steps at once
// when the answer comes: the daemon reports no progress, so there is the
// time it has run and nothing that pretends to more. The daemon diagnoses the
// route that holds the hostname, whatever owner the page shows.
export function Diagnosis({ hostname, store = diagnoses }: { hostname: string; store?: DiagnosisStore }) {
  const digest = useApp((s) => s.state?.digest)
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const d = useDiagnoses(store)
  const kept = d.kept.get(hostname)
  const failed = d.failed.get(hostname)
  const running = d.running

  const run = () => void store.run(hostname, digest ?? '', () => api<Step[]>('POST', '/api/v1/diagnose', { hostname }))

  const here = running?.hostname === hostname
  const refused = running ? (here ? 'one runs at a time' : 'another diagnosis runs; one runs at a time') : undefined
  return (
    <div className="diagnosis">
      <p className="muted">The daemon walks the chain of the route that holds this hostname, from the claim to an answer of the guest.</p>
      <div className="diagnosis-run">
        <Button variant="primary" onClick={run} disabledReason={refused}>
          Run diagnosis
        </Button>
        {here && <Busy label="Diagnosing" since={running.since} />}
      </div>
      {failed !== undefined && !here && <ErrorText error={failed} />}
      {kept && (
        <section className="diagnosis-result" aria-label="The last diagnosis">
          <p className="diagnosis-when">
            Run in this browser at <Time at={kept.at} nodeZone={nodeZone} />
            {changedSince(kept, digest) && (
              <>
                {' '}
                <Badge tone="warn">the state changed since</Badge>
              </>
            )}
          </p>
          <Stepper steps={stepsOf(kept.steps)} label="Steps of the diagnosis" />
        </section>
      )}
    </div>
  )
}
