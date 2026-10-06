import { useApp } from '../api/store'
import { Dialog } from '../components/Dialog'

export const notAffiliated = 'pco is not affiliated with Proxmox Server Solutions GmbH or Cloudflare, Inc.'

// About says which pco this is: the versions of the web process and of the
// daemon, where it runs, and the licences of the code of this page.
export function About({ open, onClose }: { open: boolean; onClose: () => void }) {
  const session = useApp((s) => s.session)
  const hello = useApp((s) => s.hello)
  const node = session?.node || undefined
  const rows: [string, string | undefined][] = [
    ['pco web', session?.version],
    ['pco daemon', hello?.version],
    ['Profile', session?.profile],
    ['Node', node],
    ['Boot of the daemon', hello?.boot],
    ['Poll interval', hello?.pollInterval],
  ]
  return (
    <Dialog open={open} onClose={onClose} title="About pco">
      <dl className="details">
        {rows.map(([name, value]) => (
          <div key={name} className="details-row">
            <dt>{name}</dt>
            <dd className="mono">{value || 'not known yet'}</dd>
          </div>
        ))}
      </dl>
      <p>
        The code of this page bundles libraries under their own licences: <a href="/licenses.txt">licenses.txt</a>.
      </p>
      <p className="muted">{notAffiliated}</p>
    </Dialog>
  )
}
