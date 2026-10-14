//go:build !linux

package lease

import (
	"errors"
	"time"
)

// SystemFS needs the cluster filesystem of a Proxmox VE node, which is Linux.
func SystemFS() FS { return unsupportedFS{} }

var errUnsupported = errors.New("the lease on the cluster filesystem needs Linux")

type unsupportedFS struct{}

func (unsupportedFS) Mkdir(string) error              { return errUnsupported }
func (unsupportedFS) Refresh(string, time.Time) error { return errUnsupported }
func (unsupportedFS) UnlockRequest(string) error      { return errUnsupported }
func (unsupportedFS) Rmdir(string) error              { return errUnsupported }
func (unsupportedFS) Entries(string) (int, error)     { return 0, errUnsupported }
