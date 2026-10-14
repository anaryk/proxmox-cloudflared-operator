package store

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Block is an address blocked on every node of a cluster: an agent saw a
// MAC bound to its guest where it must not be, on the port of another guest
// or gone from the guest's own port while the guest did not migrate. It is
// kept so that a controller that takes over holds the block for as long as
// the one that made it would have.
type Block struct {
	Addr  netip.Addr     `json:"addr"`
	Guest model.GuestRef `json:"guest"`
	MAC   string         `json:"mac"`
	// Node and Port are where the MAC was seen, and Why is conflict or
	// moved.
	Node  string    `json:"node"`
	Port  string    `json:"port,omitempty"`
	Why   string    `json:"why"`
	Since time.Time `json:"since"`
	// LastReport is when an agent last reported what blocks the address.
	LastReport time.Time `json:"lastReport"`
}

func (b Block) check() error {
	switch {
	case !b.Addr.IsValid():
		return errors.New("block has no address")
	case b.MAC == "":
		return fmt.Errorf("block of %s names no MAC", b.Addr)
	case b.Node == "":
		return fmt.Errorf("block of %s names no node", b.Addr)
	case b.Why == "":
		return fmt.Errorf("block of %s has no reason", b.Addr)
	case b.Since.IsZero(), b.LastReport.IsZero():
		return fmt.Errorf("block of %s has no time", b.Addr)
	}
	return nil
}

// blockID names the object of a block: the address, with the colons of an
// IPv6 address written as dashes, as a file name takes none.
func blockID(addr netip.Addr) string {
	return strings.ReplaceAll(addr.String(), ":", "-")
}

// Blocks returns the blocked addresses, by address. A block is read only
// from the file of its address: one that names another is an error, since
// DeleteBlock goes by the file name and would never reach it.
func (s *Store) Blocks() (map[netip.Addr]Block, error) {
	byID, err := loadMap(s.cluster, kindBlocks, "address", func(b *Block) *string {
		id := ""
		if b.Addr.IsValid() {
			id = blockID(b.Addr)
		}
		return &id
	})
	if err != nil {
		return nil, err
	}
	out := make(map[netip.Addr]Block, len(byID))
	for _, b := range byID {
		if err := b.check(); err != nil {
			return nil, err
		}
		out[b.Addr] = b
	}
	return out, nil
}

// SaveBlock stores a block under its address, unless it is what is stored
// already.
func (s *Store) SaveBlock(b Block) error {
	if err := b.check(); err != nil {
		return err
	}
	return s.cluster.put(kindBlocks, blockID(b.Addr), b, true)
}

// DeleteBlock removes the block of an address, once it ended. A missing one
// is not an error.
func (s *Store) DeleteBlock(addr netip.Addr) error {
	return s.cluster.Delete(kindBlocks, blockID(addr))
}
