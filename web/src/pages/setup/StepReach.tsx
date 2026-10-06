import { useApp } from '../../api/store'
import { docsUrl } from '../../app/docs'
import { Link } from '../../app/Link'
import { Button } from '../../components/Button'
import { Untrusted } from '../../components/Untrusted'
import { networksOf } from '../networks/networks'
import type { StepProps } from './steps'

// StepReach is the optional step about how the node reaches guests. In this
// release that is Direct only, so there is little to choose: it says what the
// node needs and shows where routes were proven.
export function StepReach({ st, go }: StepProps) {
  const version = useApp((s) => s.session?.version)
  const minimum = useApp((s) => s.settings?.settings.identityMinimum)
  const bridges = networksOf(st.routes).networks.filter((n) => n.bridge)
  return (
    <>
      <p>
        You can skip this step. pco reaches a guest directly, as the node itself, so the node needs an IPv4 address on the bridge, and the VLAN, that the network card of the
        guest is attached to. On a default install that is <span className="mono">vmbr0</span> and the management address.{' '}
        <a href={docsUrl('quickstart.md', version)}>The quickstart</a> says what to think of before giving the node an address in another network.
      </p>
      {bridges.length > 0 ? (
        <>
          <p>Routes were proven on these bridges:</p>
          <ul className="plain-list">
            {bridges.map((n) => (
              <li key={n.key}>
                <span className="mono">
                  <Untrusted text={n.bridge ?? ''} />
                </span>
                {n.vlan !== undefined && <> VLAN {n.vlan}</>}: {n.routes.length === 1 ? '1 route' : `${n.routes.length} routes`}
              </li>
            ))}
          </ul>
        </>
      ) : (
        <p className="muted">No route has been proven yet. The bridges that carry one are listed here once there is.</p>
      )}
      <p>
        pco serves a route only at the identity level <b>{minimum ?? 'of the settings'}</b> or higher, <b>port</b> being the strictest. A lower level proves less: the{' '}
        <Link to="/networks">Networks</Link> page says what each level proves, and <Link to="/settings">Settings</Link> has the choice.
      </p>
      <p className="wizard-next">
        <Button variant="primary" onClick={() => go('route')}>
          Continue to the first route
        </Button>
      </p>
    </>
  )
}
