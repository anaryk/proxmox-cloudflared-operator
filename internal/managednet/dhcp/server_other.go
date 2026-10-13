//go:build !linux

package dhcp

import "errors"

// socketOpener needs Linux: the leg is a Linux interface.
func socketOpener(Namespace) opener {
	return func(Config) (frameReader, replySender, error) {
		return nil, nil, errors.New("the DHCP server needs Linux")
	}
}
