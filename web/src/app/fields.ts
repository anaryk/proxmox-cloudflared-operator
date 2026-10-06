// Where the page shows each field of the daemon's state, or why it does not.
// A field the daemon adds has to find a home here before the tests pass, so
// that nothing the daemon says is left off the page by oversight. A home that
// begins with the name of a page the tests render, as "Doctor:", is checked
// by rendering that page.

import { stateFields } from '../api/types.gen'

export const fieldHomes: Readonly<Record<string, string>> = {
  at: 'top bar: the Live pill and the age of the data; Overview: waiting for the first cycle',
  finishedAt: 'top bar: the Live pill; when the data is stale (store.staleAfterMs)',
  node: 'top bar: the chip of the node',
  digest: 'not shown: it names the state, which the page fetches again when it changes',
  mode: 'top bar: the Mode pill; the banner of observe-only',
  complete: 'top bar: the Inventory pill; the banner of an incomplete inventory',
  routes: 'Routes, the flow map of the Overview; the Routes and Guests counters of the navigation',
  issues: 'Guests: the issues of the Notes of each guest',
  tunnels: 'Edge > Tunnels',
  connectors: 'Edge > Tunnels; the tile of the connectors on the Overview',
  credentials: 'Edge > Credentials; the Edge counter; the banner without a credential; the setup',
  zones: 'Edge > Zones; the Edge counter; the zones step of the setup',
  actions: 'Routes > Plan: the actions the last cycle did not carry out',
  conflicts: 'Routes > Plan: the records of others pco leaves alone',
  lost: 'Routes > Plan: the names that lost the marker of this install; the Needs you tile of the Overview',
  problems: 'Overview: the Problems card; top bar: the count of problems',
  writerVerdict: 'top bar: the Writer pill; the banner of a writer that is not ok',
  hold: 'the banner of a cycle that held; Routes',
  profile: 'top bar: the chip of the node; About',
  waiting: 'the banner of what waits for a confirmation; Routes > Plan',
  offer: 'not shown: a confirmation sends it back with what is on screen (Routes > Plan)',
  unapproved: 'Guests; the Guests counter; the banner of guests waiting for approval',
  segments: 'Networks',
  egress: 'top bar: the Egress pill; the egress banner with its command',
  admission: 'the empty states of Routes and the Overview; Guests',
  gateTagged: 'the empty states of Routes and the Overview',
  rogueConnectors: 'Edge > Tunnels; the Edge counter',
  identity: 'Doctor: This install, the container the appliance was installed as and whether it is this one',
  epochDrawnAt: 'Doctor: This install, the epoch drawn after the container started',
}

// homeless are the fields of names that have no home.
export function homeless(names: readonly string[] = stateFields): string[] {
  return names.filter((n) => !Object.hasOwn(fieldHomes, n))
}
