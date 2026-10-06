import { Component, type ReactNode } from 'react'

import { type AppStore, appStore } from '../api/store'
import { Empty } from '../components/Empty'

// loadPart loads a part of the page that is a chunk of its own. One that does
// not load was built for another version of pco, which has replaced this
// one: the page asks to be reloaded.
export function loadPart<T>(load: () => Promise<T>, store: AppStore = appStore): Promise<T> {
  return load().catch((e: unknown) => {
    store.markSkew()
    throw e
  })
}

interface PartProps {
  // What the part is, as the start of a sentence: "The map".
  what: string
  children: ReactNode
}

// Part holds a part of the page that loads as a chunk of its own. When the
// chunk does not load, the part says so in its place and the rest of the
// page stays; the banner above asks for the reload that brings the version
// the part was built for.
export class Part extends Component<PartProps, { failed: boolean }> {
  override state = { failed: false }

  static getDerivedStateFromError(): { failed: boolean } {
    return { failed: true }
  }

  override render(): ReactNode {
    if (!this.state.failed) return this.props.children
    return <Empty title={`${this.props.what} did not load`}>pco was updated since this page was loaded: reload the page to use the new version.</Empty>
  }
}
