import { InfoIcon } from '../components/icons'
import { Tooltip } from '../components/Tooltip'
import { Untrusted } from '../components/Untrusted'
import { countersWhy, legendWords } from '../text/flow'
import { dashes } from './edges/parts'

function Line({ className, dash }: { className: string; dash?: string }) {
  return (
    <svg className="fm-key" viewBox="0 0 28 10" aria-hidden="true">
      <path className={className} d="M1 5h26" strokeDasharray={dash} />
    </svg>
  )
}

function Dots({ error }: { error?: boolean }) {
  return (
    <svg className="fm-key" viewBox="0 0 28 10" aria-hidden="true">
      <circle className="dot" cx="5" cy="5" r="3" />
      <circle className="dot" cx="15" cy="5" r="3" />
      {error && <rect className="dot-error" x="21" y="2" width="5" height="5" transform="rotate(45 23.5 4.5)" />}
    </svg>
  )
}

export interface LegendProps {
  // Why there are no figures per target, in the daemon's words.
  why?: string
  paused?: boolean
  reduced?: boolean
  stale?: boolean
}

// Legend is the row under the map: what the dots are and where their
// figures come from, what each line style means, and what moves.
export function Legend({ why, paused, reduced, stale }: LegendProps) {
  const counters = why ? countersWhy(why) : undefined
  const sentence = stale ? legendWords.stale : reduced ? legendWords.reduced : paused ? legendWords.still : legendWords.moves
  return (
    <ul className="fm-legend" aria-label={legendWords.label}>
      <li>
        <Dots error />
        {legendWords.trunkDots}
      </li>
      {counters ? (
        <li className="fm-legend-why">
          {legendWords.noCounters} <Untrusted text={counters.short} />
          {counters.detail && (
            <Tooltip content={<Untrusted text={counters.detail} />}>
              <button type="button" className="iconbtn fm-why" aria-label={legendWords.whatWentWrong}>
                <InfoIcon />
              </button>
            </Tooltip>
          )}
        </li>
      ) : (
        <li>
          <Dots />
          {legendWords.portDots}
        </li>
      )}
      <li>
        <Line className="fm-key-plain" />
        {legendWords.served}
      </li>
      <li>
        <Line className="fm-key-unreachable" dash={dashes.unreachable} />
        {legendWords.unreachable}
      </li>
      <li>
        <Line className="fm-key-withdrawn" dash={dashes.withdrawn} />
        {legendWords.withdrawn}
      </li>
      <li>
        <Line className="fm-key-rogue" dash={dashes.rogue} />
        {legendWords.rogue}
      </li>
      <li>
        <Line className="fm-key-muted" />
        {legendWords.muted}
      </li>
      <li className="fm-legend-moves muted">{sentence}</li>
    </ul>
  )
}
