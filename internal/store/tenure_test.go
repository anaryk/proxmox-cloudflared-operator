package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// ackFive writes the acknowledgement of vmbr0 five times, to revision 5.
func ackFive(t *testing.T, s *Store) {
	t.Helper()
	for i := range 5 {
		require.NoError(t, s.SaveSegment(Segment{Bridge: "vmbr0", AcknowledgedAt: t0.Add(time.Duration(i) * time.Minute)}))
	}
}

// restored is the acknowledgement of vmbr0 as a copy of the store taken at
// revision 3 holds it.
const restored = `{"schemaVersion":1,"rev":3,"id":"vmbr0","data":{"bridge":"vmbr0","acknowledgedAt":"2026-10-01T12:02:00Z"}}`

func TestAnObjectReadAtALowerRevisionThanTheTenureWroteIsTheStoreGoingBack(t *testing.T) {
	s, p := openStore(t)
	s.StartTenure()
	ackFive(t, s)
	path := filepath.Join(p.Cluster, "segments", "vmbr0.json")
	require.EqualValues(t, 5, revOf(t, path))
	_, err := s.Segments()
	require.NoError(t, err)
	require.NoError(t, s.WentBack())

	writeFile(t, path, restored)
	got, err := s.Segments()

	require.NoError(t, err, "the read itself goes on")
	require.Len(t, got, 1)
	err = s.WentBack()
	require.ErrorIs(t, err, ErrWentBack)
	require.ErrorContains(t, err, `segments "vmbr0"`)
	require.ErrorContains(t, err, "revision 3")
	require.ErrorContains(t, err, "revision 5")
}

func TestAnObjectTheTenureWroteThatIsGoneIsTheStoreGoingBack(t *testing.T) {
	s, p := openStore(t)
	s.StartTenure()
	require.NoError(t, s.SaveSettings(DefaultSettings()))
	ackFive(t, s)
	require.NoError(t, os.Remove(filepath.Join(p.Cluster, "segments", "vmbr0.json")))

	got, err := s.Segments()
	require.NoError(t, err)
	require.Empty(t, got)
	err = s.WentBack()
	require.ErrorIs(t, err, ErrWentBack, "a listing misses it")
	require.ErrorContains(t, err, `segments "vmbr0"`)

	s.StartTenure()
	require.NoError(t, s.SaveSettings(customSettings()))
	require.NoError(t, os.Remove(filepath.Join(p.Cluster, "meta", "settings.json")))
	_, err = s.Settings()
	require.NoError(t, err)
	require.ErrorIs(t, s.WentBack(), ErrWentBack, "a read of the one object misses it")
	require.ErrorContains(t, s.WentBack(), `meta "settings"`)
}

func TestAnObjectTheTenureDeletedIsNotMissing(t *testing.T) {
	s, _ := openStore(t)
	s.StartTenure()
	ackFive(t, s)
	require.NoError(t, s.DeleteSegment("vmbr0"))
	require.NoError(t, s.SaveClaims(map[string]planner.Claim{"a.example.com": claimOf("a.example.com", "qemu/101")}))
	require.NoError(t, s.SaveClaims(map[string]planner.Claim{}))

	_, err := s.Segments()
	require.NoError(t, err)
	_, err = s.Claims()
	require.NoError(t, err)

	require.NoError(t, s.WentBack())
}

func TestAWriteOfAnotherProcessAtAHigherRevisionIsNoStepBack(t *testing.T) {
	s, p := openStore(t)
	s.StartTenure()
	ackFive(t, s)
	other, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, other.SaveSegment(Segment{Bridge: "vmbr0", AcknowledgedAt: t0.Add(time.Hour)}))

	_, err = s.Segments()
	require.NoError(t, err)
	require.NoError(t, s.WentBack())
}

func TestTheTenureCheckIsOffUntilATenureStarts(t *testing.T) {
	s, p := openStore(t)
	ackFive(t, s)
	writeFile(t, filepath.Join(p.Cluster, "segments", "vmbr0.json"), restored)

	_, err := s.Segments()
	require.NoError(t, err)
	require.NoError(t, s.WentBack(), "nothing was written in a tenure")

	s.StartTenure()
	ackFive(t, s)
	writeFile(t, filepath.Join(p.Cluster, "segments", "vmbr0.json"), restored)
	_, err = s.Segments()
	require.NoError(t, err)
	require.ErrorIs(t, s.WentBack(), ErrWentBack)

	s.StartTenure()
	require.NoError(t, s.WentBack(), "a new tenure starts clean")
	_, err = s.Segments()
	require.NoError(t, err)
	require.NoError(t, s.WentBack(), "and knows nothing of what the last one wrote")
}

// The node's own root is no part of the store a restore takes back: what
// another process of the node does there is no step back of the store.
func TestTheTenureCheckWatchesTheSharedRootsAlone(t *testing.T) {
	s, p := openStore(t)
	s.StartTenure()
	require.NoError(t, s.SaveEngineMemory(memoryOf("example.com")))
	require.NoError(t, s.SaveEngineMemory(memoryOf("example.org")))
	writeFile(t, filepath.Join(p.Local, "meta", "engine-memory.json"), envelopeJSON("engine-memory", `{"version":1}`))
	_, err := s.EngineMemory()
	require.NoError(t, err)
	require.NoError(t, s.WentBack())

	require.NoError(t, s.SaveJoinToken(joinRecord("t1", "pve-b")))
	require.NoError(t, s.SaveJoinToken(func() JoinRecord { r := joinRecord("t1", "pve-b"); r.Uses = []JoinUse{{At: t0}}; return r }()))
	writeFile(t, filepath.Join(p.Private, "jointokens", "t1.json"),
		envelopeJSON("t1", `{"id":"t1","node":"pve-b","secretSHA256":"`+sha("secret of t1")+`","createdAt":"2026-10-01T12:00:00Z","expires":"2026-10-01T12:15:00Z"}`))

	_, err = s.JoinTokens()
	require.NoError(t, err)
	require.ErrorIs(t, s.WentBack(), ErrWentBack, "a used token that is unused again is the store going back")
}

// The tenure's own writes and reads may run at once: a read that started
// before a write and ends after it is no step back.
func TestTheTenureCheckIsNotFooledByItsOwnWrites(t *testing.T) {
	s, _ := openStore(t)
	s.StartTenure()
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 200 {
			_ = s.SaveSegment(Segment{Bridge: "vmbr0", AcknowledgedAt: t0.Add(time.Duration(i) * time.Second)})
			if i%10 == 0 {
				_ = s.DeleteSegment("vmbr0")
			}
		}
	})
	wg.Go(func() {
		for range 200 {
			_, _ = s.Segments()
		}
	})
	wg.Wait()

	require.NoError(t, s.WentBack())
}
