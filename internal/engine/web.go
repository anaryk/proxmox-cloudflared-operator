package engine

// kindWeb is the kind of the events of the web interface.
const kindWeb = "web"

// NoteWeb records as an event what the daemon did for the web interface,
// such as a certificate it renewed.
func (e *Engine) NoteWeb(msg string) {
	e.events.add(Event{At: e.d.Now(), Level: levelInfo, Kind: kindWeb, Subject: "web certificate", Message: msg})
}

// NoteWebListen records as an event that the web interface of the appliance
// listens on another address of net0 now.
func (e *Engine) NoteWebListen(msg string) {
	e.events.add(Event{At: e.d.Now(), Level: levelWarn, Kind: kindWeb, Subject: "web listen", Message: msg})
}
