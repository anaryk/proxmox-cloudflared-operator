// Package registry keeps what the controller decides about the nodes of an
// install: the states a node passes through, the index its leg MACs are built
// from, whether the forwarding table of every online node is watched, and which
// routes have to move before a node that joins may take shared ones. It holds
// no clock and reads no file; the caller says what it saw and when.
package registry

import (
	"errors"
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// NoEntry is the state of a node the registry does not know yet.
const NoEntry agentproto.NodeState = ""

// Event is something that happened to a node, or that an admin or the
// controller decided about it.
type Event string

const (
	// EventJoined: the node was enrolled.
	EventJoined Event = "joined"
	// EventAdmitted: the node passed admission and may take shared routes.
	EventAdmitted Event = "admitted"
	// EventHeard: the node's agent sent a status.
	EventHeard Event = "heard"
	// EventSilent: the node's agent sent nothing for 30 seconds.
	EventSilent Event = "silent"
	// EventOffline: Proxmox lists the node as offline.
	EventOffline Event = "offline"
	// EventOnline: Proxmox lists the node as online.
	EventOnline Event = "online"
	// EventDrain: an admin, or maintenance mode, asked the node to stop
	// serving shared routes.
	EventDrain Event = "drain"
	// EventUndrain: the drain is over.
	EventUndrain Event = "undrain"
	// EventForget: an admin removed the node from the install.
	EventForget Event = "forget"
	// EventContest: a join token was used in a way that does not add up.
	EventContest Event = "contest"
	// EventReturned: the agent says it started again, which is what a reboot
	// or a new installation of the node looks like.
	EventReturned Event = "returned"
)

// ErrNoTransition is what Next wraps when an event is not one that can happen
// to a node in its state. For a command it is a refusal to show the admin; for
// an observation it is nothing to do, since the controller only ever hears a
// node it refuses, a contested or removed one, in the cycle the refusal began.
var ErrNoTransition = errors.New("not a transition of the node registry")

// transitions is every state a node can be in and what each event makes of it.
// What is missing is no transition.
var transitions = map[agentproto.NodeState]map[Event]agentproto.NodeState{
	NoEntry: {
		EventJoined:  agentproto.Joining,
		EventContest: agentproto.Contested,
	},
	agentproto.Joining: {
		EventJoined:   agentproto.Joining,
		EventAdmitted: agentproto.Active,
		EventHeard:    agentproto.Joining,
		EventSilent:   agentproto.Unknown,
		EventOffline:  agentproto.Down,
		EventOnline:   agentproto.Joining,
		EventForget:   agentproto.Removed,
		EventContest:  agentproto.Contested,
		EventReturned: agentproto.Joining,
	},
	agentproto.Active: {
		EventAdmitted: agentproto.Active,
		EventHeard:    agentproto.Active,
		EventSilent:   agentproto.Unknown,
		EventOffline:  agentproto.Down,
		EventOnline:   agentproto.Active,
		EventDrain:    agentproto.Draining,
		EventUndrain:  agentproto.Active,
		EventForget:   agentproto.Removed,
		EventContest:  agentproto.Contested,
		EventReturned: agentproto.Joining,
	},
	agentproto.Draining: {
		EventHeard:    agentproto.Draining,
		EventSilent:   agentproto.Unknown,
		EventOffline:  agentproto.Down,
		EventOnline:   agentproto.Draining,
		EventDrain:    agentproto.Draining,
		EventUndrain:  agentproto.Active,
		EventForget:   agentproto.Removed,
		EventContest:  agentproto.Contested,
		EventReturned: agentproto.Joining,
	},
	agentproto.Unknown: {
		EventHeard:    agentproto.Joining,
		EventSilent:   agentproto.Unknown,
		EventOffline:  agentproto.Down,
		EventOnline:   agentproto.Unknown,
		EventForget:   agentproto.Removed,
		EventContest:  agentproto.Contested,
		EventReturned: agentproto.Joining,
	},
	agentproto.Down: {
		EventHeard:    agentproto.Down,
		EventSilent:   agentproto.Down,
		EventOffline:  agentproto.Down,
		EventOnline:   agentproto.Unknown,
		EventForget:   agentproto.Removed,
		EventContest:  agentproto.Contested,
		EventReturned: agentproto.Down,
	},
	agentproto.Contested: {
		EventSilent:  agentproto.Contested,
		EventOffline: agentproto.Contested,
		EventOnline:  agentproto.Contested,
		EventForget:  agentproto.Removed,
		EventContest: agentproto.Contested,
	},
	agentproto.Removed: {
		EventJoined:  agentproto.Joining,
		EventSilent:  agentproto.Removed,
		EventOffline: agentproto.Removed,
		EventOnline:  agentproto.Removed,
		EventForget:  agentproto.Removed,
		EventContest: agentproto.Contested,
	},
}

// Next is the state of a node after ev, or an error wrapping ErrNoTransition
// when ev is not something that can happen to a node in state.
//
// A node is enrolled Joining and takes shared routes only once it is admitted.
// One that went silent, one that Proxmox listed as offline and one that
// rebooted come back through Joining, so that admission looks at its
// networking again. Down follows what Proxmox says: an agent that reports
// while the cluster lists its node as offline does not end it. A drain does
// not survive a node's absence either; the controller asks for it again when
// it still wants it. Contested and Removed are left by an admin only: forget
// ends the first, enrolling the node again the second. A join token misused
// for the name of a removed node contests it, as it would a name without an
// entry, so that a replacement whose token was taken shows up as such.
//
// What the agent of a contested or removed node says, heard or returned, is no
// transition: the controller refuses such a node, and a status that still
// arrives in the cycle of the refusal is ignored, not an error of the cycle.
//
// An event that leaves a node where it already is, as seeing the same thing
// every cycle does, and a command repeated after a crash, are transitions to
// the same state. A node whose entry is not there yet has NoEntry as its state.
func Next(state agentproto.NodeState, ev Event) (agentproto.NodeState, error) {
	if next, ok := transitions[state][ev]; ok {
		return next, nil
	}
	return "", fmt.Errorf("%s cannot take %q: %w", describe(state), ev, ErrNoTransition)
}

func describe(state agentproto.NodeState) string {
	if state == NoEntry {
		return "a node without an entry"
	}
	if _, ok := transitions[state]; ok {
		return fmt.Sprintf("a %s node", state)
	}
	return fmt.Sprintf("a node in state %q", state)
}
