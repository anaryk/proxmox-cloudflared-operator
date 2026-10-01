package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

func stoneOf(offset time.Duration, generation int) reconcile.Tombstone {
	return reconcile.Tombstone{Since: t0.Add(offset), Seen: t0.Add(offset + time.Minute), Generation: generation}
}

func TestTombstonesLoadOfAMissingFileIsAnEmptyMap(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Tombstones().Load(t.Context())
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
	require.Empty(t, stored(t, p.Cluster), "a load writes nothing")
}

func TestTombstonesRoundTrip(t *testing.T) {
	s, p := openStore(t)
	want := map[string]reconcile.Tombstone{
		"zone1/shop.example.com":  stoneOf(0, 3),
		"zone1/*.example.com":     stoneOf(time.Hour, 3),
		"zone2/other.example.org": stoneOf(2*time.Hour, 4),
	}
	require.NoError(t, s.Tombstones().Save(t.Context(), want))

	got, err := s.Tombstones().Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, []string{"meta/tombstones.json"}, stored(t, p.Cluster))
	require.Empty(t, stored(t, p.Local))
	require.Empty(t, stored(t, p.Private))

	got["zone9/x.example.com"] = stoneOf(0, 1)
	again, err := s.Tombstones().Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, again, "a loaded map is the caller's own")
}

func TestTombstonesSaveWritesOnlyWhenTheMapDiffers(t *testing.T) {
	s, p := openStore(t)
	path := filepath.Join(p.Cluster, "meta", "tombstones.json")
	m := map[string]reconcile.Tombstone{"zone1/a.example.com": stoneOf(0, 1)}
	require.NoError(t, s.Tombstones().Save(t.Context(), m))
	require.EqualValues(t, 1, revOf(t, path))

	require.NoError(t, s.Tombstones().Save(t.Context(), m))
	loaded, err := s.Tombstones().Load(t.Context())
	require.NoError(t, err)
	require.NoError(t, s.Tombstones().Save(t.Context(), loaded))
	require.EqualValues(t, 1, revOf(t, path))

	m["zone1/b.example.com"] = stoneOf(time.Hour, 1)
	require.NoError(t, s.Tombstones().Save(t.Context(), m))
	require.EqualValues(t, 2, revOf(t, path))

	seen := m["zone1/a.example.com"]
	seen.Seen = seen.Seen.Add(time.Minute)
	m["zone1/a.example.com"] = seen
	require.NoError(t, s.Tombstones().Save(t.Context(), m))
	require.EqualValues(t, 3, revOf(t, path))

	delete(m, "zone1/b.example.com")
	require.NoError(t, s.Tombstones().Save(t.Context(), m))
	require.EqualValues(t, 4, revOf(t, path))
}

func TestTombstonesSaveOfNothingStoresAnEmptyMap(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.Tombstones().Save(t.Context(), map[string]reconcile.Tombstone{"zone1/a.example.com": stoneOf(0, 1)}))
	require.NoError(t, s.Tombstones().Save(t.Context(), nil))

	got, err := s.Tombstones().Load(t.Context())
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
	require.JSONEq(t, `{}`, string(readRaw(t, filepath.Join(p.Cluster, "meta", "tombstones.json")).Data))
}

func TestTombstonesLoadOfAnUnreadableFileIsAnError(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "meta", "tombstones.json"), `{"schemaVersion":1,"rev":1,"id":"tombstones","data":{"a":`)
	got, err := s.Tombstones().Load(t.Context())
	require.Error(t, err)
	require.Nil(t, got)
	require.Contains(t, err.Error(), "tombstones.json")
}

func TestTombstonesStopOnACancelledContext(t *testing.T) {
	s, p := openStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := s.Tombstones().Load(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, s.Tombstones().Save(ctx, map[string]reconcile.Tombstone{"z/a.example.com": stoneOf(0, 1)}), context.Canceled)
	require.Empty(t, stored(t, p.Cluster))
}

func adoptedSample(i int) cfapi.Record {
	return cfapi.Record{
		ID:         "rec-" + string(rune('a'+i)),
		Type:       "CNAME",
		Name:       "shop.example.com",
		Content:    "other.example.net",
		Proxied:    true,
		Comment:    "set by hand",
		ModifiedOn: t0.Add(-time.Hour),
	}
}

type adoptedLine struct {
	At     time.Time       `json:"at"`
	Zone   string          `json:"zone"`
	Record json.RawMessage `json:"record"`
}

// adoptedLines reads adopted.jsonl line by line; every line must be a JSON object.
func adoptedLines(t *testing.T, s *Store) []adoptedLine {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.paths.Cluster, "adopted.jsonl"))
	require.NoError(t, err)
	require.True(t, len(b) == 0 || b[len(b)-1] == '\n', "every line ends with a newline")
	var out []adoptedLine
	for line := range bytes.Lines(b) {
		var l adoptedLine
		require.NoError(t, json.Unmarshal(line, &l), string(line))
		out = append(out, l)
	}
	return out
}

func TestAppendAdoptedWritesOneJSONLinePerRecord(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.AppendAdopted(t0, "example.com", adoptedSample(0)))
	require.NoError(t, s.AppendAdopted(t0.Add(time.Minute), "example.org", adoptedSample(1)))

	b, err := os.ReadFile(filepath.Join(p.Cluster, "adopted.jsonl"))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	require.Len(t, lines, 2)
	require.JSONEq(t, `{
		"at": "2026-10-01T12:00:00Z",
		"zone": "example.com",
		"record": {
			"id": "rec-a", "type": "CNAME", "name": "shop.example.com", "content": "other.example.net",
			"proxied": true, "comment": "set by hand", "modifiedOn": "2026-10-01T11:00:00Z"
		}
	}`, lines[0])

	got := adoptedLines(t, s)
	require.Len(t, got, 2)
	require.Equal(t, "example.org", got[1].Zone)
	require.Equal(t, t0.Add(time.Minute), got[1].At)
	require.Equal(t, []string{"adopted.jsonl"}, stored(t, p.Cluster))
}

func TestAppendAdoptedWritesTimesInUTC(t *testing.T) {
	s, _ := openStore(t)
	zone := time.FixedZone("CEST", 2*3600)
	require.NoError(t, s.AppendAdopted(t0.In(zone), "example.com", adoptedSample(0)))
	got := adoptedLines(t, s)
	require.Len(t, got, 1)
	require.True(t, got[0].At.Equal(t0))

	b, err := os.ReadFile(filepath.Join(s.paths.Cluster, "adopted.jsonl"))
	require.NoError(t, err)
	require.Contains(t, string(b), `"at":"2026-10-01T12:00:00Z"`)
}

func TestAppendAdoptedOmitsWhatTheRecordDoesNotHave(t *testing.T) {
	s, _ := openStore(t)
	require.NoError(t, s.AppendAdopted(t0, "example.com", cfapi.Record{ID: "r", Type: "A", Name: "a.example.com", Content: "192.0.2.1"}))
	got := adoptedLines(t, s)
	require.JSONEq(t, `{"id":"r","type":"A","name":"a.example.com","content":"192.0.2.1","proxied":false}`, string(got[0].Record))
}

func TestAppendAdoptedDropsTheOldestLinesAtTheCap(t *testing.T) {
	s, p := openStore(t)
	path := filepath.Join(p.Cluster, "adopted.jsonl")
	rec := adoptedSample(0)
	rec.Comment = strings.Repeat("c", 4000)

	const lines = 120 // about 480 KiB written in all
	for i := range lines {
		rec.ID = "rec-" + string(rune('A'+i%26)) + strings.Repeat("x", i%7)
		require.NoError(t, s.AppendAdopted(t0.Add(time.Duration(i)*time.Second), "example.com", rec))
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.LessOrEqual(t, info.Size(), int64(256<<10), "after line %d", i)
	}

	got := adoptedLines(t, s)
	require.Less(t, len(got), lines)
	require.Greater(t, len(got), 50, "the cap is reached by size, not by count")
	require.True(t, got[len(got)-1].At.Equal(t0.Add((lines-1)*time.Second)), "the newest line is kept")
	for i := 1; i < len(got); i++ {
		require.True(t, got[i].At.Equal(got[i-1].At.Add(time.Second)), "the lines kept are consecutive, oldest dropped")
	}
	require.Equal(t, []string{"adopted.jsonl"}, stored(t, p.Cluster), "no temporary file is left")
}

func TestAppendAdoptedKeepsTheNewestLineWhateverItsSize(t *testing.T) {
	s, _ := openStore(t)
	require.NoError(t, s.AppendAdopted(t0, "example.com", adoptedSample(0)))
	big := adoptedSample(1)
	big.Content = strings.Repeat("x", 300<<10)

	require.NoError(t, s.AppendAdopted(t0.Add(time.Second), "example.com", big))
	got := adoptedLines(t, s)
	require.Len(t, got, 1)
	require.True(t, got[0].At.Equal(t0.Add(time.Second)))
}

func TestAppendAdoptedContinuesAFileWithoutAFinalNewline(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "adopted.jsonl"), `{"at":"2026-10-01T11:00:00Z","zone":"example.com","record":{}}`)
	require.NoError(t, s.AppendAdopted(t0, "example.com", adoptedSample(0)))
	require.Len(t, adoptedLines(t, s), 2)
}

func TestAppendAdoptedFailsWhenTheLogCannotBeRead(t *testing.T) {
	s, p := openStore(t)
	// A directory in place of the log: neither read nor replaced.
	writeFile(t, filepath.Join(p.Cluster, "adopted.jsonl", "inner"), "x")
	require.Error(t, s.AppendAdopted(t0, "example.com", adoptedSample(0)))
	requireMissing(t, filepath.Join(p.Cluster, "adopted.jsonl.tmp"))
}
