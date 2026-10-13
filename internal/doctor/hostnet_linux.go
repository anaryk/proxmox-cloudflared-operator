package doctor

import (
	"errors"
	"fmt"
	"io/fs"

	"golang.org/x/sys/unix"
)

// netnsPinned checks that path is the pin of a network namespace: a mount of
// nsfs, not the empty file that stays when the namespace was unmounted.
func netnsPinned(path string) error {
	var st unix.Statfs_t
	err := unix.Statfs(path, &st)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s does not exist", path)
	case err != nil:
		return fmt.Errorf("%s: %w", path, err)
	case st.Type != unix.NSFS_MAGIC:
		return fmt.Errorf("%s is no namespace", path)
	}
	return nil
}
