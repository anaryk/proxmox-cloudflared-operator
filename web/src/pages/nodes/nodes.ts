// What the pages of the cluster work out from the state by themselves:
// whether there is a cluster to show, the order of the nodes, what needs a
// person, and the words of a replica, a placement and a move.

import type { MoveView, NodeView, PlacementView, ReplicaView, State } from '../../api/types.gen'
import type { Tone } from '../../components/icons'
import { durationText, nanosToMs } from '../../text/duration'
import { compareNodeStates } from '../../text/words'

type ClusterState = Pick<State, 'node' | 'nodes' | 'cluster' | 'lease'>

// clustered says whether the state is a cluster's: the registry has a node,
// the controller described the cluster, or this node does not hold the
// lease. A node alone shows no page of the cluster.
export function clustered(st: ClusterState | undefined): boolean {
  if (!st) return false
  return st.nodes.length > 0 || st.cluster !== undefined || notController(st)
}

// notController says whether this node does not hold the lease: another
// node is the controller, or none is known. The page then shows what this
// node's agent knows, and every action is refused here.
export function notController(st: Pick<State, 'node' | 'lease'> | undefined): boolean {
  const l = st?.lease
  if (!l) return false
  return !l.valid && l.holder !== st?.node
}

const compareText = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0)

// compareNodes is the order of pco node list: by state, then by name.
export function compareNodes(a: NodeView, b: NodeView): number {
  return compareNodeStates(a.state, b.state) || compareText(a.name, b.name)
}

// The states of a node that need a person: one that waits for admission,
// one whose join token was misused, one whose agent went quiet.
const needing = new Set(['joining', 'contested', 'unknown'])

export const nodeNeeds = (n: Pick<NodeView, 'state'>): boolean => needing.has(n.state)

// nodesNeeding counts what the navigation shows for the nodes: the nodes
// that need a person, and those the new CA refuses after a rotation.
export function nodesNeeding(st: ClusterState | undefined): number {
  if (!st) return 0
  return st.nodes.filter(nodeNeeds).length + (st.cluster?.caRotation?.refused?.length ?? 0)
}

// nodeTone is the colour of a node's state; the word always goes with it.
export function nodeTone(state: string): Tone {
  switch (state) {
    case 'active':
      return 'ok'
    case 'joining':
    case 'draining':
      return 'info'
    case 'unknown':
      return 'warn'
    case 'down':
    case 'contested':
      return 'fail'
  }
  return 'idle'
}

// kindText names what runs the agent: the host, or the container of an
// appliance agent.
export function kindText(n: Pick<NodeView, 'kind' | 'vmid'>): string {
  if (n.kind === 'appliance') return n.vmid ? `appliance, container ${n.vmid}` : 'appliance'
  return n.kind || 'host'
}

// skewText says how far a node's clock is from the controller's, as the
// controller read it from the last status: nothing when they agree.
export function skewText(ns: number): string {
  const ms = Math.round(nanosToMs(ns))
  if (ms === 0) return 'none'
  const abs = Math.abs(ms)
  const text = abs < 1000 ? `${abs} ms` : durationText(abs)
  return ms > 0 ? `+${text} (ahead)` : `-${text} (behind)`
}

// targetsText counts the targets of a node's desired state: verified,
// failing, below the identity minimum, and the routes pinned to it.
export function targetsText(n: Pick<NodeView, 'verified' | 'failing' | 'belowMinimum' | 'pinned'>): string {
  const parts = [`${n.verified} verified`]
  if (n.failing > 0) parts.push(`${n.failing} failing`)
  if (n.belowMinimum > 0) parts.push(`${n.belowMinimum} below the minimum`)
  if (n.pinned > 0) parts.push(`${n.pinned} pinned`)
  return parts.join(' · ')
}

// A fingerprint typed to admit an appliance agent: the SHA-256 of its
// certificate in hex, at least 8 digits of it, as the daemon checks it
// before it compares. fingerprintIssue is what is wrong with one, or
// nothing.
export function fingerprintIssue(text: string): string | undefined {
  const fp = text.trim()
  if (fp === '') return undefined
  if (!/^[0-9a-fA-F:]+$/.test(fp)) return 'a fingerprint is the SHA-256 of the certificate in hex: digits 0-9 and a-f only'
  const digits = fp.replace(/:/g, '')
  if (digits.length < 8) return 'at least 8 hex digits of the fingerprint are needed'
  if (digits.length > 64) return 'a SHA-256 has 64 hex digits; this has more'
  return undefined
}

// fingerprintOf is the fingerprint as it is sent: lower case, without the
// colons a certificate viewer puts between the bytes.
export const fingerprintOf = (text: string): string => text.trim().replace(/:/g, '').toLowerCase()

// A node is named as Proxmox names it: a label of a host name.
export function nodeNameIssue(text: string): string | undefined {
  const name = text.trim()
  if (name === '') return 'a node is named'
  if (!/^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(name)) return 'a node name is lower-case letters, digits and hyphens between them, as Proxmox names it'
  return undefined
}

// replicaLevelText is the level a replica proved: port (attested by pve1)
// when the forwarding table of pve1, the node that runs the guest, vouched
// for the port. An attestation is never shown without its node, so a port
// another node saw is not read as one this replica proved.
export function replicaLevelText(r: Pick<ReplicaView, 'level' | 'attested' | 'attestedBy'>): string {
  if (!r.level) return '-'
  return r.attested && r.attestedBy ? `${r.level} (attested by ${r.attestedBy})` : r.level
}

// replicaTone is the colour of a replica: reaching the target and holding
// it in its egress set, reaching it but not serving it yet, or failing.
export function replicaTone(r: Pick<ReplicaView, 'ok' | 'inSet'>): Tone {
  if (r.ok && r.inSet) return 'ok'
  if (r.ok) return 'warn'
  return 'fail'
}

// placementText says where a route is placed: the shared tunnel, or the
// tunnel pinned to a node, and why.
export function placementText(p: PlacementView): string {
  if (p.kind === 'pinned') return `pinned to ${p.node ?? ''} (tunnel ${p.tunnel})${p.why ? `: ${p.why}` : ''}`
  return `shared tunnel ${p.tunnel}${p.why ? `: ${p.why}` : ''}`
}

const stepWords: Readonly<Record<string, string>> = {
  'rules-added': 'the rules are in the tunnel moved to',
  'rolled-out': 'its connectors run the configuration with them',
  'cnames-switched': 'the records point at the tunnel moved to; the old rules go five minutes later',
}

// moveText says where a move between tunnels is.
export function moveText(m: MoveView): string {
  return `moving from ${m.from} to ${m.to}: ${stepWords[m.step] ?? m.step}`
}

// takeoverHold is the line of a controller that took over and waits for
// the agents before it writes anything at Cloudflare, when the problems have
// one.
export function takeoverHold(st: Pick<State, 'problems'> | undefined): string | undefined {
  return st?.problems.find((p) => p.startsWith('waiting for the agents of ') || p.startsWith('the agents of '))
}

// Links of the pages of the cluster.
export const nodeLink = (name: string): string => `/nodes/${encodeURIComponent(name)}`
