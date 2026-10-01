package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// tunnelDir is the directory of the local root that holds the files of the
// tunnel connectors. Its token files are the ones systemd hands to
// cloudflared, and the connector manager writes them too: both write the token
// as it is, so neither sees a difference where there is none.
const tunnelDir = "tunnels"

var tunnelIDPattern = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// tunnelTokenPath returns the token file of a tunnel and the temporary file
// it is written through. The temporary file is hidden, as the connector
// manager's are, so that nothing that lists the connectors sees it.
func (s *Store) tunnelTokenPath(tunnelID string) (dir, path, tmp string, err error) {
	if !tunnelIDPattern.MatchString(tunnelID) {
		return "", "", "", fmt.Errorf("invalid tunnel id %q", tunnelID)
	}
	dir = filepath.Join(s.paths.Local, tunnelDir)
	return dir, filepath.Join(dir, tunnelID+".token"), filepath.Join(dir, "."+tunnelID+".token"+tempExt), nil
}

// TunnelToken returns the token of a tunnel, trimmed of white space. A missing
// file, or one with nothing in it, holds no token.
func (s *Store) TunnelToken(tunnelID string) (string, bool, error) {
	_, path, _, err := s.tunnelTokenPath(tunnelID)
	if err != nil {
		return "", false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading the token of tunnel %s: %w", tunnelID, err)
	}
	token := strings.TrimSpace(string(b))
	return token, token != "", nil
}

// SaveTunnelToken writes the token file of a tunnel, readable by root only. The
// token is written as it is, so it must not start or end with white space.
func (s *Store) SaveTunnelToken(tunnelID, token string) error {
	dir, path, tmp, err := s.tunnelTokenPath(tunnelID)
	if err != nil {
		return err
	}
	if token == "" || strings.TrimSpace(token) != token {
		return fmt.Errorf("the token of tunnel %s is empty or has white space around it", tunnelID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureDir(dir); err != nil {
		return fmt.Errorf("saving the token of tunnel %s: %w", tunnelID, err)
	}
	if err := writeFileAtomic(path, tmp, []byte(token)); err != nil {
		return fmt.Errorf("saving the token of tunnel %s: %w", tunnelID, err)
	}
	return nil
}

// DeleteTunnelToken removes the token file of a tunnel. A missing one is not
// an error.
func (s *Store) DeleteTunnelToken(tunnelID string) error {
	_, path, _, err := s.tunnelTokenPath(tunnelID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return removeIfThere(path)
}
