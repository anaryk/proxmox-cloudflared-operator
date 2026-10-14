package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/rs/zerolog"
)

const (
	// defaultHoldCycle is the file whose presence holds a cycle while the
	// test hooks are on.
	defaultHoldCycle = "/run/pco/hold-cycle"
	// holdLookEvery is how often a held cycle looks whether the file is
	// still there.
	holdLookEvery = time.Second
)

// holdHook is what a cycle begins with while the test hooks are on: it
// holds for as long as the file is there, until the daemon stops. It ignores
// the deadline of the cycle, as a step that hangs would: the suites see the
// watchdog restart a daemon that makes no progress. nil without the hooks,
// where the file means nothing.
func holdHook(cfg Config, sleep func(context.Context, time.Duration) error, log zerolog.Logger) func(context.Context) {
	if !cfg.TestHooks {
		return nil
	}
	path := cfg.HoldCycle
	if path == "" {
		path = defaultHoldCycle
	}
	return func(ctx context.Context) {
		said := false
		for {
			if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
				return
			}
			if !said {
				log.Warn().Str("file", path).Msg("the cycle is held while the file exists (PCO_TEST_HOOKS)")
				said = true
			}
			if err := sleep(ctx, holdLookEvery); err != nil {
				return
			}
		}
	}
}
