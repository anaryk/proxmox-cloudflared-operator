// The PNG files of the screenshots. They read what Chromium writes, 8 bits a
// channel in RGB or RGBA, not interlaced, and what recompress writes.

import { crc32, deflateSync, inflateSync } from 'node:zlib'

const signature = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])

interface Image {
  width: number
  height: number
  channels: number
  px: Buffer // the rows, unfiltered, one after the other
}

function chunk(type: string, data: Buffer): Buffer {
  const head = Buffer.alloc(8)
  head.writeUInt32BE(data.length, 0)
  head.write(type, 4, 'latin1')
  const crc = Buffer.alloc(4)
  crc.writeUInt32BE(crc32(Buffer.concat([head.subarray(4), data])), 0)
  return Buffer.concat([head, data, crc])
}

// paeth is the predictor of filter 4.
function paeth(a: number, b: number, c: number): number {
  const p = a + b - c
  const pa = Math.abs(p - a)
  const pb = Math.abs(p - b)
  const pc = Math.abs(p - c)
  return pa <= pb && pa <= pc ? a : pb <= pc ? b : c
}

const at = (b: Buffer, i: number) => b[i] ?? 0

function decode(png: Buffer): Image {
  if (!png.subarray(0, 8).equals(signature)) throw new Error('not a PNG')
  let width = 0
  let height = 0
  let channels = 0
  const idat: Buffer[] = []
  for (let i = 8; i < png.length; ) {
    const length = png.readUInt32BE(i)
    const type = png.toString('latin1', i + 4, i + 8)
    const data = png.subarray(i + 8, i + 8 + length)
    if (type === 'IHDR') {
      width = data.readUInt32BE(0)
      height = data.readUInt32BE(4)
      const [depth, colour, , , interlace] = data.subarray(8, 13)
      if (depth !== 8 || (colour !== 2 && colour !== 6) || interlace !== 0) {
        throw new Error(`a PNG of depth ${depth}, colour type ${colour}, interlace ${interlace}: want 8, 2 or 6, 0`)
      }
      channels = colour === 6 ? 4 : 3
    } else if (type === 'IDAT') {
      idat.push(data)
    }
    i += 12 + length
  }
  const stride = width * channels
  const raw = inflateSync(Buffer.concat(idat))
  const px = Buffer.alloc(height * stride)
  for (let y = 0; y < height; y++) {
    const filter = at(raw, y * (stride + 1))
    const row = y * (stride + 1) + 1
    for (let x = 0; x < stride; x++) {
      const a = x >= channels ? at(px, y * stride + x - channels) : 0
      const b = y > 0 ? at(px, (y - 1) * stride + x) : 0
      const c = x >= channels && y > 0 ? at(px, (y - 1) * stride + x - channels) : 0
      const predicted = [0, a, b, (a + b) >> 1, paeth(a, b, c)][filter]
      if (predicted === undefined) throw new Error(`row ${y} has filter ${filter}`)
      px[y * stride + x] = (at(raw, row + x) + predicted) & 0xff
    }
  }
  return { width, height, channels, px }
}

// recompress writes a screenshot again, the same pixels in fewer bytes:
// rows without a filter and zlib at its best compression make the flat
// colours of the interface about a quarter smaller, which keeps the images
// of docs/ within their budget. An RGBA image whose pixels are all opaque is
// written as RGB.
export function recompress(png: Buffer): Buffer {
  const { width, height, channels, px } = decode(png)
  let opaque = channels === 4
  for (let i = 3; opaque && i < px.length; i += 4) opaque = px[i] === 0xff
  const out = opaque ? 3 : channels
  const rows = Buffer.alloc(height * (width * out + 1))
  for (let y = 0, o = 0; y < height; y++) {
    rows[o++] = 0
    for (let x = 0; x < width; x++) {
      const p = (y * width + x) * channels
      for (let k = 0; k < out; k++) rows[o++] = at(px, p + k)
    }
  }
  const header = Buffer.alloc(13)
  header.writeUInt32BE(width, 0)
  header.writeUInt32BE(height, 4)
  header.set([8, out === 4 ? 6 : 2, 0, 0, 0], 8)
  return Buffer.concat([signature, chunk('IHDR', header), chunk('IDAT', deflateSync(rows, { level: 9, memLevel: 9 })), chunk('IEND', Buffer.alloc(0))])
}

// samePicture says whether two screenshots show the same: of the same size,
// and no colour of a pixel more than one step apart. Chromium under an
// emulator of amd64 now and then rounds the edge of a rounded corner the
// other way; that is no change of the interface.
export function samePicture(a: Buffer, b: Buffer): boolean {
  if (a.equals(b)) return true
  const x = decode(a)
  const y = decode(b)
  if (x.width !== y.width || x.height !== y.height || x.channels !== y.channels) return false
  for (let i = 0; i < x.px.length; i++) {
    if (Math.abs(at(x.px, i) - at(y.px, i)) > 1) return false
  }
  return true
}
