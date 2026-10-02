package daemon

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

func TestSocketAccess(t *testing.T) {
	webUser := &user.User{Uid: "998", Gid: "997", Username: webName}
	webGroup := &user.Group{Gid: "997", Name: webName}

	for _, tt := range []struct {
		name     string
		accounts fakeAccounts
		gid      int
		uids     []uint32
		logged   string
	}{
		{"no pco-web at all", fakeAccounts{}, 0, []uint32{0}, ""},
		{"user and group", fakeAccounts{user: webUser, group: webGroup}, 997, []uint32{0, 998}, ""},
		{"only the group", fakeAccounts{group: webGroup}, 997, []uint32{0}, ""},
		{"only the user", fakeAccounts{user: webUser}, 0, []uint32{0, 998}, ""},
		{
			"a group id that is not a number", fakeAccounts{group: &user.Group{Gid: "web", Name: webName}},
			0, []uint32{0}, "the group id is not a number",
		},
		{
			"a user id that is not a number", fakeAccounts{user: &user.User{Uid: "web", Username: webName}},
			0, []uint32{0}, "the user id is not a number",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := &testutil.SyncBuffer{}

			gid, uids := socketAccess(tt.accounts, zerolog.New(logs))

			require.Equal(t, tt.gid, gid)
			require.Equal(t, tt.uids, uids)
			if tt.logged == "" {
				require.Empty(t, logs.String(), "a missing user or group is normal")
			} else {
				require.Contains(t, logs.String(), tt.logged)
			}
		})
	}
}

// brokenAccounts fails every lookup the way a system with a broken NSS does.
type brokenAccounts struct{}

func (brokenAccounts) LookupUser(string) (*user.User, error)   { return nil, errors.New("nss is down") }
func (brokenAccounts) LookupGroup(string) (*user.Group, error) { return nil, errors.New("nss is down") }

func TestSocketAccessFallsBackToRootWhenALookupFails(t *testing.T) {
	logs := &testutil.SyncBuffer{}

	gid, uids := socketAccess(brokenAccounts{}, zerolog.New(logs))

	require.Equal(t, 0, gid)
	require.Equal(t, []uint32{0}, uids)
	require.Contains(t, logs.String(), "looking up the group failed")
	require.Contains(t, logs.String(), "looking up the user failed")
}

func TestWiredSettingsNameWhatChanged(t *testing.T) {
	base := wired{gateTag: "cf-tunnel", trustedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}

	for _, tt := range []struct {
		name string
		now  wired
		want []string
	}{
		{"nothing", base, nil},
		{"the gate tag", wired{gateTag: "publish", trustedCIDRs: base.trustedCIDRs}, []string{"gateTag"}},
		{"static trust", wired{gateTag: "cf-tunnel", trustStatic: true, trustedCIDRs: base.trustedCIDRs}, []string{"trustStatic"}},
		{"the trusted networks", wired{gateTag: "cf-tunnel"}, []string{"trustedCIDRs"}},
		{
			"all of it", wired{gateTag: "x", trustStatic: true},
			[]string{"gateTag", "trustStatic", "trustedCIDRs"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, base.differences(tt.now))
		})
	}
}

func TestTheTokenIsReadAgainOnlyWhileTheClusterFilesystemIsGone(t *testing.T) {
	w := newWorld(t)
	log := zerolog.Nop()
	noSleep := func(context.Context, time.Duration) error { return errors.New("must not wait") }

	t.Run("a token that is there", func(t *testing.T) {
		tok, err := readPVEToken(t.Context(), w.store, noSleep, log)
		require.NoError(t, err)
		require.Equal(t, pveTokenID, tok.TokenID)
		require.Equal(t, pveSecret, tok.Secret.Reveal())
	})

	t.Run("a token file that cannot be read is no reason to wait", func(t *testing.T) {
		path := filepath.Join(w.paths.Private, "meta", "pve-token.json")
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
		t.Cleanup(func() { require.NoError(t, os.WriteFile(path, raw, 0o600)) })

		_, err = readPVEToken(t.Context(), w.store, noSleep, log)

		require.ErrorContains(t, err, "reading the Proxmox API token")
	})

	t.Run("the wait ends with the context", func(t *testing.T) {
		require.NoError(t, os.Remove(w.mount))
		ctx, cancel := context.WithCancel(t.Context())
		sleep := func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}

		_, err := readPVEToken(ctx, w.store, sleep, log)

		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestSleepContextEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, sleepContext(ctx, time.Hour), context.Canceled)
	require.NoError(t, sleepContext(t.Context(), time.Nanosecond))
}
