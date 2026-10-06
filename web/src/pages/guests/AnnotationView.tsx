import { Fragment } from 'react'

import type { AnnotationView as Annotation, Issue } from '../../api/types.gen'
import { Untrusted } from '../../components/Untrusted'

// caret is the row under a line that points at a column, counted from 1.
export const caret = (col: number | undefined) => `${' '.repeat(Math.max((col ?? 1) - 1, 0))}^`

const position = (is: Issue) => (is.col ? `line ${is.line}, column ${is.col}` : `line ${is.line}`)

// AnnotationView shows the route text of a guest's Notes as the daemon cut
// it out, with the numbers of the lines in the Notes, and under each line
// with an issue a caret at its column and the message: "line 5, column 5" is
// something one can see. The text is the guest's, so it is shown as
// untrusted text.
export function AnnotationView({ view }: { view: Annotation }) {
  if (view.startLine <= 0 || view.block === '') {
    return <p className="muted">The Notes of this guest have no route text.</p>
  }
  const lines = view.block.split('\n')
  const last = view.startLine + lines.length - 1
  const elsewhere = view.issues.filter((is) => !is.line || is.line < view.startLine || is.line > last)
  return (
    <>
      <pre className="annotation" aria-label={`The route text of the Notes of ${view.ref}, from line ${view.startLine}`}>
        {lines.map((text, at) => {
          const n = view.startLine + at
          return (
            <Fragment key={n}>
              <span className="annotation-line">
                <span className="annotation-no">{n}</span>
                <Untrusted text={text} />
              </span>
              {'\n'}
              {view.issues
                .filter((is) => is.line === n)
                .map((is, k) => (
                  <Fragment key={k}>
                    <span className="annotation-caret" aria-hidden="true">
                      <span className="annotation-no" />
                      {caret(is.col)}
                    </span>
                    {'\n'}
                    <span className="annotation-msg">
                      <span className="annotation-no" />
                      <span className="sr-only">{position(is)}: </span>
                      <Untrusted text={is.msg} />
                    </span>
                    {'\n'}
                  </Fragment>
                ))}
            </Fragment>
          )
        })}
      </pre>
      {elsewhere.length > 0 && (
        <ul className="plain-list">
          {elsewhere.map((is, k) => (
            <li key={k}>
              {is.line ? `${position(is)}: ` : ''}
              <Untrusted text={is.msg} />
            </li>
          ))}
        </ul>
      )}
    </>
  )
}
