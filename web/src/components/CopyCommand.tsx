import { useState } from 'react'

import { type CommandWords, isCommand } from '../text/words'
import { Button } from './Button'
import { CopyIcon, WarnIcon } from './icons'
import { Untrusted } from './Untrusted'

// CopyCommand offers a command for a root shell on the node, or says why
// there is none. It takes only what words.ts composes: a constant, or a
// command whose every value passed commandArg (spec-ui 9.6), so nothing a
// guest or Cloudflare wrote can reach the shell by way of the page. Anything
// else, cast past the compiler, is refused here.
export function CopyCommand({ cmd, root }: { cmd: CommandWords; root?: boolean }) {
  const [copied, setCopied] = useState<'copied' | 'failed'>()
  if (!isCommand(cmd.command)) {
    const why = cmd.command === undefined ? cmd.refused : 'it was not composed from checked values'
    return (
      <p className="command-refused">
        <WarnIcon />
        <span>
          No command to copy: <Untrusted text={why} />
        </span>
      </p>
    )
  }
  const command = cmd.command.text
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command)
      setCopied('copied')
    } catch {
      setCopied('failed')
    }
  }
  return (
    <div className="command">
      <div className="command-line">
        <pre className="snippet">
          <code>{command}</code>
        </pre>
        <Button small icon={<CopyIcon />} onClick={() => void copy()}>
          Copy
        </Button>
      </div>
      <p className="command-note">
        {root && 'Run it as root on the node. '}
        <span role="status">
          {copied === 'copied' && 'Copied.'}
          {copied === 'failed' && 'The browser did not let the page copy it: select the command and copy it yourself.'}
        </span>
      </p>
    </div>
  )
}
