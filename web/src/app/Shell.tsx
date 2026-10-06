import { useState } from 'react'

import { About } from './About'
import { Activity } from './Activity'
import { Banners } from './Banners'
import { EventsStrip } from './EventsStrip'
import { Nav } from './Nav'
import { Page } from './Page'
import type { View } from './router'
import { SignInDialog } from './SignInDialog'
import { TopBar } from './TopBar'

const collapsedKey = 'pco.nav'

function readCollapsed(): boolean {
  try {
    return window.localStorage.getItem(collapsedKey) === 'icons'
  } catch {
    return false
  }
}

function writeCollapsed(on: boolean): void {
  try {
    if (on) window.localStorage.setItem(collapsedKey, 'icons')
    else window.localStorage.removeItem(collapsedKey)
  } catch {
    // kept for this page only
  }
}

// Shell is every page of a signed-in user: the top bar, the
// navigation, the banners over the content and the events strip under it.
export function Shell({ view }: { view: View }) {
  const [navOpen, setNavOpen] = useState(false)
  const [collapsed, setCollapsed] = useState(readCollapsed)
  const [about, setAbout] = useState(false)
  // A phone's navigation is a drawer: going somewhere closes it.
  const [shown, setShown] = useState(view)
  if (shown !== view && shown.name !== view.name) {
    setShown(view)
    setNavOpen(false)
  }

  return (
    <div className="shell">
      <a className="skiplink" href="#main">
        Skip to content
      </a>
      <TopBar navOpen={navOpen} onNav={() => setNavOpen(!navOpen)} onAbout={() => setAbout(true)} />
      <div className={collapsed ? 'layout layout-collapsed' : 'layout'}>
        <Nav
          open={navOpen}
          collapsed={collapsed}
          onCollapse={() => {
            writeCollapsed(!collapsed)
            setCollapsed(!collapsed)
          }}
          onAbout={() => setAbout(true)}
        />
        <div className="content">
          <main id="main" tabIndex={-1}>
            <Banners />
            <Page view={view} />
          </main>
          <EventsStrip />
        </div>
      </div>
      <About open={about} onClose={() => setAbout(false)} />
      <SignInDialog />
      <Activity />
    </div>
  )
}
