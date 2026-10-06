import type { Report } from '../../api/types.gen'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'

// The question of pco credential check --deep.
export const deepQuestion = 'A deep check creates and deletes a test DNS record and a test tunnel. Continue?'

// Above this many zones a deep check may not finish within the minute the
// daemon waits for it.
export const deepZoneLimit = 12

// deepLimitText says how long a deep check of the zones and accounts of the
// last report may take, when that is more than the daemon waits: it makes
// about three calls per zone and three per account, one after the other.
export function deepLimitText(report: Pick<Report, 'zones' | 'accounts'> | undefined): string {
  const zones = report?.zones.length ?? 0
  if (zones <= deepZoneLimit) return ''
  const calls = 3 * zones + 3 * (report?.accounts.length ?? 0)
  return `A deep check of ${zones} zones makes about ${calls} calls to Cloudflare and may not finish within the minute the daemon waits; a token scoped to fewer zones checks faster.`
}

// DeepCheckDialog asks before a check that writes, in the words of the
// command line, and says when it may not finish in time; it can be run
// anyway.
export function DeepCheckDialog({ open, report, onConfirm, onClose }: { open: boolean; report?: Pick<Report, 'zones' | 'accounts'>; onConfirm: () => void; onClose: () => void }) {
  const limit = deepLimitText(report)
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="Check write access"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={onConfirm}>
            Check write access
          </Button>
        </>
      }
    >
      <p>{deepQuestion}</p>
      {limit && <p className="warn-text">{limit}</p>}
    </Dialog>
  )
}
