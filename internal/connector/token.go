package connector

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Token returns the token the connector of a tunnel is run with, as Ensure last
// wrote it. A tunnel without a token file, or with a blank one, has none.
//
// The token file is the manager's alone: a token that anything else put there
// would make the next Ensure see identical bytes and so leave the running
// connector on the old token. The file is read under the manager's lock, so an
// answer never comes from the middle of an Ensure. No error carries the token.
func (m *Manager) Token(tunnelID string) (token string, found bool, err error) {
	if err := checkID(tunnelID); err != nil {
		return "", false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	b, err := m.readFile(m.path(tokenFile(tunnelID)))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading the token of tunnel %s: %w", tunnelID, err)
	}
	token = strings.TrimSpace(string(b))
	return token, token != "", nil
}
