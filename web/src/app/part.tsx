import { Component, type ReactNode } from 'react'

import { type AppStore, appStore } from '../api/store'
import { Empty } from '../components/Empty'

// NotLoaded is what loadPart throws for a chunk that did not load, so that
// the part that waited for it can tell it from an error of its own code.
export class NotLoaded extends Error {
  constructor(cause: unknown) {
    super('a part of the page did not load', { cause })
    this.name = 'NotLoaded'
  }
}

// loadPart loads a part of the page that is a chunk of its own. One that does
// not load was built for another version of pco, which has replaced this
// one: the page asks to be reloaded.
export function loadPart<T>(load: () => Promise<T>, store: AppStore = appStore): Promise<T> {
  return load().catch((e: unknown) => {
    store.markSkew()
    throw new NotLoaded(e)
  })
}

interface PartProps {
  // What the part is, as the start of a sentence: "The map".
  what: string
  children: ReactNode
}

type Failure = 'not-loaded' | 'not-drawn' | undefined

// Part holds a part of the page that loads as a chunk of its own. When the
// chunk does not load, the part says so in its place and the rest of the
// page stays; the banner above asks for the reload that brings the version
// the part was built for. Code of the part that fails as it draws is said
// in words of its own: no other version would draw it.
export class Part extends Component<PartProps, { failed: Failure }> {
  override state: { failed: Failure } = { failed: undefined }

  static getDerivedStateFromError(e: unknown): { failed: Failure } {
    return { failed: e instanceof NotLoaded ? 'not-loaded' : 'not-drawn' }
  }

  override render(): ReactNode {
    const { what, children } = this.props
    switch (this.state.failed) {
      case 'not-loaded':
        return <Empty title={`${what} did not load`}>pco was updated since this page was loaded: reload the page to use the new version.</Empty>
      case 'not-drawn':
        return <Empty title={`${what} could not be drawn`}>This is an error of the page. Reloading it may help; the console of the browser says what went wrong.</Empty>
    }
    return children
  }
}
