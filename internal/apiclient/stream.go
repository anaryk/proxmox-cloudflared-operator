package apiclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// maxMessage bounds one message of the stream.
const maxMessage = 4 << 20

var errMessageTooLarge = errors.New("a message of the stream is too large")

// Stream follows the stream of the daemon: it returns its hello, then the
// notices on the channel until ctx ends or the daemon ends the stream, when
// the channel is closed. With the boot and the seq of the last event a client
// saw, the daemon replays what came after it, or begins with a reset when it
// runs as another process now. The hello has to come within the short
// timeout.
func (c *Client) Stream(ctx context.Context, boot string, after uint64) (<-chan engine.Notice, engine.Hello, error) {
	caller := ctx
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/stream", nil)
	if err != nil {
		cancel()
		return nil, engine.Hello{}, fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if boot != "" {
		req.Header.Set("Last-Event-ID", fmt.Sprintf("%s:%d", boot, after))
	}
	timer := time.AfterFunc(c.short, cancel)
	res, err := c.http.Do(req)
	if err != nil {
		late := !timer.Stop()
		cancel()
		if late && caller.Err() == nil {
			err = context.DeadlineExceeded
		}
		return nil, engine.Hello{}, c.transportError(caller, err, c.short)
	}
	defer func() {
		if err != nil {
			_ = res.Body.Close()
			cancel()
		}
	}()
	hello, r, err := c.begin(res)
	switch late := !timer.Stop(); {
	case caller.Err() != nil:
		err = c.transportError(caller, caller.Err(), c.short)
	case late:
		err = c.transportError(caller, context.DeadlineExceeded, c.short)
	}
	if err != nil {
		return nil, engine.Hello{}, err
	}
	out := make(chan engine.Notice)
	go func() {
		defer cancel()
		defer func() { _ = res.Body.Close() }()
		defer close(out)
		r.relay(ctx, out)
	}()
	return out, hello, nil
}

// relay hands the notices of the stream on until it ends, ctx ends or a
// notice cannot be read; what follows that is not trusted.
func (s *sseReader) relay(ctx context.Context, out chan<- engine.Notice) {
	for {
		m, err := s.next()
		if err != nil {
			return
		}
		n, known, err := noticeOf(m)
		switch {
		case err != nil:
			return
		case !known:
			continue
		}
		select {
		case out <- n:
		case <-ctx.Done():
			return
		}
	}
}

// begin checks the answer to a request for the stream and reads its hello.
func (c *Client) begin(res *http.Response) (engine.Hello, *sseReader, error) {
	if res.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(res.Body, maxMessage))
		return engine.Hello{}, nil, c.statusError(res.StatusCode, data)
	}
	if mt, _, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err != nil || mt != "text/event-stream" {
		return engine.Hello{}, nil, &daemonError{msg: "the answer of the pco daemon is not a stream", noAnswer: true}
	}
	r := &sseReader{r: bufio.NewReader(res.Body)}
	m, err := r.next()
	var hello engine.Hello
	if err != nil || m.event != "hello" || json.Unmarshal(m.data, &hello) != nil {
		return engine.Hello{}, nil, &daemonError{msg: "the stream of the pco daemon did not begin with its hello", noAnswer: true}
	}
	return hello, r, nil
}

// noticeOf is the notice a message of the stream carries; known is false for
// a kind of message the client does not know, which it skips.
func noticeOf(m sseEvent) (n engine.Notice, known bool, err error) {
	n.Kind = m.event
	switch m.event {
	case engine.NoticeState:
		n.State = new(engine.StateNotice)
		err = json.Unmarshal(m.data, n.State)
	case engine.NoticeEvent:
		n.Event = new(engine.Event)
		err = json.Unmarshal(m.data, n.Event)
	case engine.NoticeGap:
		n.Gap = new(engine.GapNotice)
		err = json.Unmarshal(m.data, n.Gap)
	case engine.NoticeTraffic:
		n.Traffic = new(engine.TrafficNotice)
		err = json.Unmarshal(m.data, n.Traffic)
	case engine.NoticeReset:
		var reset struct {
			Reason string `json:"reason"`
		}
		err = json.Unmarshal(m.data, &reset)
		n.Reason = reset.Reason
	default:
		return engine.Notice{}, false, nil
	}
	return n, true, err
}

// sseEvent is one message of a stream of server-sent events. Its id is not
// kept: the notices carry the boot and the seq themselves.
type sseEvent struct {
	event string
	data  []byte
}

// sseReader reads the messages of a stream of server-sent events. A line
// ends in LF or CRLF; the data lines of a message are joined by LF.
type sseReader struct {
	r *bufio.Reader
}

// next returns the next message that has data; comments, and messages
// without data, are skipped.
func (s *sseReader) next() (sseEvent, error) {
	var m sseEvent
	var data bytes.Buffer
	hasData, size := false, 0
	for {
		line, err := s.line()
		if err != nil {
			return sseEvent{}, err
		}
		if len(line) == 0 {
			if hasData {
				m.data = data.Bytes()
				return m, nil
			}
			m, size = sseEvent{}, 0
			continue
		}
		if size += len(line); size > maxMessage {
			return sseEvent{}, errMessageTooLarge
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			m.event = string(value)
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.Write(value)
			hasData = true
		}
	}
}

// line reads one line without its end.
func (s *sseReader) line() ([]byte, error) {
	var line []byte
	for {
		part, more, err := s.r.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, part...)
		if len(line) > maxMessage {
			return nil, errMessageTooLarge
		}
		if !more {
			return line, nil
		}
	}
}
