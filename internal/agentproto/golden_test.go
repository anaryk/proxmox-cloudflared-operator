package agentproto

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

// The goldens of testdata/v1 are protocol 1. A later build that speaks it must
// write the same JSON, give or take fields it adds, and read these files back
// to the same values.
func TestTheGoldensPinProtocolOne(t *testing.T) {
	t.Run("snapshot", func(t *testing.T) {
		s := fullSnapshot()
		requireEveryFieldSet(t, s)
		var buf bytes.Buffer
		require.NoError(t, EncodeSnapshot(&buf, s))
		require.Contains(t, buf.String(), sharedToken)
		require.Contains(t, buf.String(), pinnedToken)
		golden := requireGolden(t, "snapshot.json", buf.Bytes())

		got, err := DecodeSnapshot(bytes.NewReader(golden))
		require.NoError(t, err)
		require.Equal(t, s, got)
		require.Equal(t, sharedToken, got.Tunnels[0].Token.Reveal())
	})
	t.Run("snapshot as a log shows it", func(t *testing.T) {
		b, err := json.Marshal(fullSnapshot())
		require.NoError(t, err)
		require.NotContains(t, string(b), sharedToken)
		require.NotContains(t, string(b), pinnedToken)
		golden := requireGolden(t, "snapshot_logged.json", b)

		_, err = DecodeSnapshot(bytes.NewReader(golden))
		require.ErrorContains(t, err, "redacted", "a logged snapshot must not be applied with its placeholder for a token")
	})
	t.Run("status", func(t *testing.T) {
		st := fullStatus()
		requireEveryFieldSet(t, st)
		b, err := json.Marshal(st)
		require.NoError(t, err)
		golden := requireGolden(t, "status.json", b)

		got, err := DecodeStatus(bytes.NewReader(golden))
		require.NoError(t, err)
		require.Equal(t, st, got)
	})
	t.Run("heartbeat", func(t *testing.T) { requireRoundTrip(t, "heartbeat.json", fullHeartbeat()) })
	t.Run("agent view", func(t *testing.T) { requireRoundTrip(t, "agent_view.json", fullAgentView()) })
	t.Run("pve write", func(t *testing.T) { requireRoundTrip(t, "pve_write.json", fullPVEWrite()) })
	t.Run("refusal", func(t *testing.T) { requireRoundTrip(t, "refusal.json", fullRefusal()) })
	t.Run("probe", func(t *testing.T) { requireRoundTrip(t, "probe.json", fullProbe()) })
	t.Run("probe answer", func(t *testing.T) { requireRoundTrip(t, "probe_answer.json", fullProbeAnswer()) })
}

func TestAnEmptySnapshotSurvivesTheWire(t *testing.T) {
	for _, s := range []Snapshot{{}, {Tunnels: []Tunnel{{TunnelID: sharedID}}}} {
		var buf bytes.Buffer
		require.NoError(t, EncodeSnapshot(&buf, s))
		require.NotContains(t, buf.String(), `"token"`)
		got, err := DecodeSnapshot(&buf)
		require.NoError(t, err)
		require.Equal(t, s, got)
	}
}

// A controller of a later build may add optional fields within protocol 1, so
// the readers must take them and go on.
func TestReadersIgnoreFieldsTheyDoNotKnow(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, EncodeSnapshot(&buf, fullSnapshot()))
	body := addUnknownFields(t, buf.String(), `"protocol":`, `"accountId":`, `"key":`)
	s, err := DecodeSnapshot(strings.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, fullSnapshot(), s)

	b, err := json.Marshal(fullStatus())
	require.NoError(t, err)
	body = addUnknownFields(t, string(b), `"protocol":`, `"key":`, `"tunnelId":`)
	st, err := DecodeStatus(strings.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, fullStatus(), st)
}

func addUnknownFields(t *testing.T, body string, before ...string) string {
	t.Helper()
	for _, key := range before {
		require.Contains(t, body, key)
		body = strings.Replace(body, key, `"addedLater":{"n":[1,2]},`+key, 1)
	}
	return body
}

func TestADecoderRefusesWhatFollowsTheValue(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, EncodeSnapshot(&buf, Snapshot{Node: "node2"}))
	buf.WriteString(`{"node":"node3"}`)
	_, err := DecodeSnapshot(&buf)
	require.Error(t, err)

	_, err = DecodeSnapshot(strings.NewReader(""))
	require.Error(t, err)
}

func requireRoundTrip[T any](t *testing.T, name string, v T) {
	t.Helper()
	requireEveryFieldSet(t, v)
	b, err := json.Marshal(v)
	require.NoError(t, err)
	golden := requireGolden(t, name, b)
	var got T
	require.NoError(t, json.Unmarshal(golden, &got))
	require.Equal(t, v, got)
}

// requireGolden checks body, indented, against the golden file of protocol 1
// and returns the file, compacted. A field already pinned may never go away or
// change; a new one is pinned with -update.
func requireGolden(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.Indent(&buf, bytes.TrimSpace(body), "", "  "))
	buf.WriteByte('\n')
	path := filepath.Join("testdata", "v1", name)
	if old, err := os.ReadFile(path); err == nil {
		lost := lostFields(t, old, buf.Bytes())
		require.Empty(t, lost, "fields of protocol 1 went away or changed; a build of protocol 1 may only add fields")
	}
	if *update {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), buf.String(), "the JSON of protocol 1 gained a field; run the test with -update to pin it")
	var compact bytes.Buffer
	require.NoError(t, json.Compact(&compact, want))
	return compact.Bytes()
}

// lostFields returns the paths of the values of old that are not in now as
// they were: a key gone, a value changed, a list of another length.
func lostFields(t *testing.T, old, now []byte) []string {
	t.Helper()
	return lost(decodeAny(t, old), decodeAny(t, now), "$")
}

func decodeAny(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	require.NoError(t, dec.Decode(&v))
	return v
}

func lost(old, now any, path string) []string {
	switch o := old.(type) {
	case map[string]any:
		n, ok := now.(map[string]any)
		if !ok {
			return []string{path}
		}
		var out []string
		for _, k := range slices.Sorted(maps.Keys(o)) {
			v, ok := n[k]
			if !ok {
				out = append(out, path+"."+k)
				continue
			}
			out = append(out, lost(o[k], v, path+"."+k)...)
		}
		return out
	case []any:
		n, ok := now.([]any)
		if !ok || len(n) != len(o) {
			return []string{path}
		}
		var out []string
		for i := range o {
			out = append(out, lost(o[i], n[i], fmt.Sprintf("%s[%d]", path, i))...)
		}
		return out
	}
	if !reflect.DeepEqual(old, now) {
		return []string{path}
	}
	return nil
}

// requireEveryFieldSet fails for a field of this package's types that v leaves
// at its zero value, so that the golden of v pins every field. A field of a
// list or a map counts as set when one element sets it.
func requireEveryFieldSet(t *testing.T, v any) {
	t.Helper()
	missing := slices.Sorted(maps.Keys(unset(reflect.ValueOf(v), reflect.TypeOf(v).Name())))
	require.Empty(t, missing, "the fixture leaves fields unset, so the golden does not pin them")
}

var ownPackage = reflect.TypeFor[Snapshot]().PkgPath()

func unset(v reflect.Value, path string) map[string]bool {
	if v.IsZero() {
		return map[string]bool{path: true}
	}
	switch v.Kind() {
	case reflect.Pointer:
		return unset(v.Elem(), path)
	case reflect.Slice:
		if v.Type().Elem().PkgPath() != ownPackage {
			return nil
		}
		var each []map[string]bool
		for i := range v.Len() {
			each = append(each, unset(v.Index(i), path+"[]"))
		}
		return inAll(each)
	case reflect.Map:
		if v.Type().Elem().PkgPath() != ownPackage {
			return nil
		}
		var each []map[string]bool
		for it := v.MapRange(); it.Next(); {
			each = append(each, unset(it.Value(), path+"[]"))
		}
		return inAll(each)
	case reflect.Struct:
		if v.Type().PkgPath() != ownPackage {
			return nil
		}
		out := map[string]bool{}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.IsExported() {
				maps.Copy(out, unset(v.Field(i), path+"."+f.Name))
			}
		}
		return out
	}
	return nil
}

func inAll(sets []map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range sets[0] {
		if !slices.ContainsFunc(sets, func(s map[string]bool) bool { return !s[k] }) {
			out[k] = true
		}
	}
	return out
}
