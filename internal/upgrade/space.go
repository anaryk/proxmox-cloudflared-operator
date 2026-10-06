package upgrade

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// storeRoom is what an upgrade leaves free on the state volume, which it
// shares with the store: a full volume stops the daemon's writes too.
const storeRoom = 32 << 20

// freeSpace is what an unprivileged writer could still write in dir; root
// gets the reserved blocks on top, which the store may need.
func freeSpace(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("reading the free space of %s: %w", dir, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// roomFor refuses to write size bytes into dir for what unless storeRoom is
// left beside them.
func roomFor(free func(string) (uint64, error), dir, what string, size uint64) error {
	have, err := free(dir)
	if err != nil {
		return err
	}
	if need := size + storeRoom; have < need {
		return fmt.Errorf("not enough room in %s %s: %d MiB free, %d MiB needed with the %d MiB kept free for the store",
			dir, what, have>>20, (need+1<<20-1)>>20, storeRoom>>20)
	}
	return nil
}
