package setup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// describeWeb lists what uninstall removes of the web interface.
func (u *uninstall) describeWeb() {
	m := u.manifest
	if u.found.webUnit {
		u.ask.Info("  the web interface: %s is stopped and disabled", webUnit)
	}
	if m.WebEnv {
		u.ask.Info("  %s", u.host.webEnv)
	}
	if len(m.WebTLS) > 0 {
		u.ask.Info("  the certificate of the web interface: %s", strings.Join(m.WebTLS, ", "))
	}
}

// removeWeb stops the web interface and removes what setup made for it, the
// key of its certificate among it: the files and links first, then the
// directories, which stay when something else is in them.
func (u *uninstall) removeWeb(ctx context.Context) {
	if u.found.webUnit {
		if _, err := u.run.Run(ctx, "systemctl", "disable", "--now", webUnit); err != nil {
			u.fail("stopping %s: %v", webUnit, err)
			return
		}
		u.ask.Info("%s: stopped and disabled", webUnit)
	}
	m := u.manifest
	for i := len(m.WebTLS) - 1; i >= 0; i-- {
		u.removeWebPath(m.WebTLS[i])
	}
	if m.WebEnv {
		u.removeFile(u.host.webEnv)
	}
}

// removeWebPath removes one thing setup made for the certificate. Only what
// lies in the parent of the directory of the certificate is touched, whatever
// the manifest lists.
func (u *uninstall) removeWebPath(listed string) {
	root := filepath.Dir(u.host.webDir)
	path := filepath.Clean(listed)
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		u.ask.Warn("the manifest lists %s, which is not below %s; it is left as it is", listed, root)
		return
	}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return
	case err != nil:
		u.fail("looking at %s: %v", path, err)
		return
	case info.IsDir():
		if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
			u.ask.Info("%s is kept: it holds files setup did not make", path)
			return
		}
	}
	u.removeFile(path)
}
