import type { State } from '../api/types.gen'
import { WarnIcon } from '../components/icons'
import { Untrusted } from '../components/Untrusted'
import { nextStep } from '../text/words'

// Problems is the card of the Overview that pco status ends with: every
// problem line as the daemon wrote it, in its order, and the next step when
// there is one. Before the first cycle the lines are the
// standing problems the daemon started with.
export function Problems({ state }: { state: State }) {
  if (state.problems.length === 0) return null
  const next = nextStep(state)
  return (
    <section className="card" id="problems" aria-labelledby="problems-head" tabIndex={-1}>
      <div className="card-head">
        <WarnIcon />
        <h2 id="problems-head">{state.problems.length === 1 ? '1 problem' : `${state.problems.length} problems`}</h2>
      </div>
      <div className="card-body">
        <ul className="problems">
          {state.problems.map((p) => (
            <li key={p}>
              <Untrusted text={p} />
            </li>
          ))}
        </ul>
        {next && <p className="next-step">{next}</p>}
      </div>
    </section>
  )
}
