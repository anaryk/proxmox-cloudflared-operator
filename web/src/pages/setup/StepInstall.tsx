import { useApp } from '../../api/store'
import { docsUrl } from '../../app/docs'
import { CopyCommand } from '../../components/CopyCommand'
import { Untrusted } from '../../components/Untrusted'
import { setupCommand } from '../../text/words'
import type { StepProps } from './steps'

// StepInstall is shown only while the daemon cannot work: the install of the
// node is not finished, which only root on the node can change.
export function StepInstall({ st }: Pick<StepProps, 'st'>) {
  const version = useApp((s) => s.session?.version)
  const lines = st.problems.filter((p) => p.includes('pco setup'))
  return (
    <>
      <p>
        The daemon cannot do its work yet, because the install of this node is not finished. The command below sets it up. It has to run as root on the node, so this
        page cannot run it for you; the page looks again after every cycle of the daemon.
      </p>
      {lines.length > 0 && (
        <ul className="problems">
          {lines.map((line, at) => (
            <li key={`${at}:${line}`}>
              <Untrusted text={line} />
            </li>
          ))}
        </ul>
      )}
      {lines.length === 0 && <p>The daemon cannot tell who writes the tunnel configuration of this node.</p>}
      <CopyCommand cmd={setupCommand} root />
      <p>
        <a href={docsUrl('quickstart.md', version)}>What the install needs and does</a>
      </p>
    </>
  )
}
