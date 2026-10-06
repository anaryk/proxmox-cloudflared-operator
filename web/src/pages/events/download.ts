// The name of the file of an export: pco-events-<node>-<yyyymmdd>-<hhmmss>.json,
// with the time of the browser. The node's name is a name of Proxmox VE, and
// what is not a letter, digit, dot or dash in it becomes a dash.
export function exportName(node: string | undefined, now: Date): string {
  const p = (n: number, width = 2) => String(n).padStart(width, '0')
  const day = `${p(now.getFullYear(), 4)}${p(now.getMonth() + 1)}${p(now.getDate())}`
  const time = `${p(now.getHours())}${p(now.getMinutes())}${p(now.getSeconds())}`
  const name = (node ?? '').replace(/[^A-Za-z0-9.-]/g, '-')
  return `pco-events-${name ? `${name}-` : ''}${day}-${time}.json`
}

// How long the file stays to be had after the download began: some browsers
// read it only once the user chose where to save it.
const keptFor = 60_000

// download hands the text to the browser as a file.
export function download(name: string, text: string): void {
  const url = URL.createObjectURL(new Blob([text], { type: 'application/json' }))
  const link = document.createElement('a')
  link.href = url
  link.download = name
  document.body.append(link)
  link.click()
  link.remove()
  setTimeout(() => URL.revokeObjectURL(url), keptFor)
}
