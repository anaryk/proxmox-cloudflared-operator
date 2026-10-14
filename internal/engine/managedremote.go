package engine

import (
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// sentTo notes that a change of a card of g was handed to the agent of its
// node: the guest waits for it, which msg says once as an event, and a later
// cycle reads the configuration it left.
func (s *managedStep) sentTo(owner string, g model.Guest, msg string) {
	s.wait(owner, fmt.Sprintf("waits for the agent of node %s", g.Node))
	if s.c.e.writes.sayOnce(owner + " " + msg) {
		s.event(levelInfo, owner, msg)
	}
}

// noAgent says of a consenting guest that waits on a node without an
// admitted agent why its card does not come.
func (s *managedStep) noAgent(d managednet.Decision) {
	ref, err := model.ParseGuestRef(d.Owner)
	if err != nil {
		return
	}
	g, ok := s.c.snap.Guest(ref)
	if !ok || d.Why != fmt.Sprintf(managednet.NoAgentWhy, g.Node) {
		return
	}
	_, consented := s.consents[d.Owner]
	_, attached := s.attachments[d.Owner]
	if consented || attached || g.HasTag(managednet.Tag) {
		s.c.problem(problemNoAgentForGuest, d.Owner, g.Node)
	}
}
