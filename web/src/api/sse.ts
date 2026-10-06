// A reader of server-sent events over fetch. EventSource hides the status of
// the answer and reconnects on its own terms; the stream needs both, so it
// parses the text itself, as the HTML standard says: lines end in CRLF, LF or
// CR, data lines join with LF, a blank line ends a message, a line that begins
// with a colon is a comment.

export interface SseMessage {
  event: string // "message" when the server named none
  data: string
  // The id of the last event, of this message or of one before it: what a
  // client sends as Last-Event-ID to resume.
  id: string
}

export class SseParser {
  #buffer = ''
  #event = ''
  #data: string[] = []
  #id = ''
  #started = false
  // A CR ended the last chunk: an LF that begins the next belongs to it.
  #afterCR = false

  constructor(lastId = '') {
    this.#id = lastId
  }

  push(chunk: string): SseMessage[] {
    let text = chunk
    if (!this.#started && text.length > 0) {
      this.#started = true
      if (text.startsWith('\uFEFF')) text = text.slice(1)
    }
    if (this.#afterCR && text.startsWith('\n')) text = text.slice(1)
    this.#afterCR = false
    this.#buffer += text

    const out: SseMessage[] = []
    let at = 0
    for (;;) {
      const end = this.#buffer.slice(at).search(/[\r\n]/)
      if (end < 0) break
      const line = this.#buffer.slice(at, at + end)
      let next = at + end + 1
      if (this.#buffer[at + end] === '\r') {
        if (next === this.#buffer.length) this.#afterCR = true
        else if (this.#buffer[next] === '\n') next++
      }
      at = next
      const msg = this.#line(line)
      if (msg) out.push(msg)
    }
    this.#buffer = this.#buffer.slice(at)
    return out
  }

  #line(line: string): SseMessage | undefined {
    if (line === '') return this.#dispatch()
    if (line.startsWith(':')) return undefined
    const colon = line.indexOf(':')
    const field = colon < 0 ? line : line.slice(0, colon)
    let value = colon < 0 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    switch (field) {
      case 'event':
        this.#event = value
        break
      case 'data':
        this.#data.push(value)
        break
      case 'id':
        if (!value.includes('\0')) this.#id = value
        break
    }
    return undefined
  }

  #dispatch(): SseMessage | undefined {
    const event = this.#event || 'message'
    const data = this.#data
    this.#event = ''
    this.#data = []
    if (data.length === 0) return undefined
    return { event, data: data.join('\n'), id: this.#id }
  }
}

// readSse yields the messages of a body until it ends; a message the body
// did not finish is dropped, as the standard says.
export async function* readSse(body: ReadableStream<Uint8Array>, lastId = ''): AsyncGenerator<SseMessage> {
  const parser = new SseParser(lastId)
  const decoder = new TextDecoder()
  const reader = body.getReader()
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      yield* parser.push(decoder.decode(value, { stream: true }))
    }
  } finally {
    reader.releaseLock()
  }
}
