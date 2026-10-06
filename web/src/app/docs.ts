// Links into the documentation of the repository, at the tag of the version
// that runs, so that a page of an older pco does not show the syntax of a
// newer one; a build of no release links the main branch.

const repository = 'https://github.com/anaryk/proxmox-cloudflared-operator'

// The versions a release has: 1.3.0, v1.3.0, v1.4.0-rc.1. What git describe
// writes for a commit after a tag (v1.3.0-5-g1a2b3c4) is not one.
const release = /^v?\d+\.\d+\.\d+(-(alpha|beta|rc)\.\d+)?$/

export function docsUrl(page: string, version?: string): string {
  const ref = version && release.test(version) ? (version.startsWith('v') ? version : `v${version}`) : 'main'
  return `${repository}/blob/${ref}/docs/${page}`
}
