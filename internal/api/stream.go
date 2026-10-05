package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	pingEvery = 15 * time.Second
	// writeWindow is how long one write of a stream may take. Every write
	// sets it anew, so that the write timeout of the server, which is for a
	// whole answer, does not end the stream.
	writeWindow = 30 * time.Second
)

var errBadResume = &httpError{http.StatusBadRequest, codeInvalid,
	"Last-Event-ID must be <boot>:<seq>, as the id of an event of the stream has it", false}

// getStream sends the hello, then every notice of the engine as a
// server-sent event and a comment every 15 s, until the client goes, the
// engine ends the stream or the server shuts down. Last-Event-ID, or the
// query lastEventId, resumes after an event.
func (s *Server) getStream(c *gin.Context) {
	boot, after, err := resumeFrom(c)
	if err != nil {
		s.fail(c, err)
		return
	}
	ctx := c.Request.Context()
	notices, hello, err := s.engine.Subscribe(ctx, boot, after)
	if err != nil {
		s.fail(c, err)
		return
	}
	hello.Version = s.version
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	out := sseWriter{w: c.Writer, rc: http.NewResponseController(c.Writer)}
	if out.send(sseMessage("", "hello", hello)) != nil {
		return
	}
	ping, stop := s.ticker(pingEvery)
	defer stop()
	closing := closingOf(ctx)
	for {
		var msg []byte
		select {
		case n, ok := <-notices:
			if !ok {
				return
			}
			msg = noticeMessage(n)
		case <-ping:
			msg = []byte(": ping\n\n")
		case <-closing:
			return
		case <-ctx.Done():
			return
		}
		if out.send(msg) != nil {
			return
		}
	}
}

// resumeFrom reads where a client resumes: the boot and the seq of the last
// event it saw, or nothing.
func resumeFrom(c *gin.Context) (boot string, after uint64, err error) {
	id := c.GetHeader("Last-Event-ID")
	if id == "" {
		id = c.Query("lastEventId")
	}
	if id == "" {
		return "", 0, nil
	}
	boot, seq, _ := strings.Cut(id, ":")
	after, err = strconv.ParseUint(seq, 10, 64)
	if err != nil || !isBoot(boot) {
		return "", 0, errBadResume
	}
	return boot, after, nil
}

// isBoot reports whether s has the form of a boot: 16 lower-case hex digits.
func isBoot(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := range len(s) {
		if strings.IndexByte("0123456789abcdef", s[i]) < 0 {
			return false
		}
	}
	return true
}

// noticeMessage is the message of a notice on the wire; nil for one that has
// none.
func noticeMessage(n engine.Notice) []byte {
	switch {
	case n.Kind == engine.NoticeState && n.State != nil:
		return sseMessage("", "state", n.State)
	case n.Kind == engine.NoticeEvent && n.Event != nil:
		return sseMessage(eventID(n.Event.Boot, n.Event.Seq), "event", n.Event)
	case n.Kind == engine.NoticeGap && n.Gap != nil:
		return sseMessage(eventID(n.Gap.Boot, n.Gap.To), "gap", n.Gap)
	case n.Kind == engine.NoticeTraffic && n.Traffic != nil:
		return sseMessage("", "traffic", n.Traffic)
	case n.Kind == engine.NoticeReset:
		return sseMessage("", "reset", struct {
			Reason string `json:"reason"`
		}{n.Reason})
	}
	return nil
}

func eventID(boot string, seq uint64) string { return fmt.Sprintf("%s:%d", boot, seq) }

// sseMessage is one message of the stream, with a blank line after it. The
// JSON of data has no line break, so one data line carries it.
func sseMessage(id, event string, data any) []byte {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	var b bytes.Buffer
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("event: " + event + "\n")
	b.WriteString("data: ")
	b.Write(payload)
	b.WriteString("\n\n")
	return b.Bytes()
}

// sseWriter writes the messages of a stream, each at once.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (s sseWriter) send(msg []byte) error {
	if len(msg) == 0 {
		return nil
	}
	// A writer that cannot take a deadline has no write timeout either.
	_ = s.rc.SetWriteDeadline(time.Now().Add(writeWindow))
	if _, err := s.w.Write(msg); err != nil {
		return err
	}
	return s.rc.Flush()
}
