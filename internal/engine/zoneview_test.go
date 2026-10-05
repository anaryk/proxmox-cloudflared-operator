package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

var (
	exampleCom = cfapi.Zone{ID: "zone1", Name: "example.com", Status: "active", AccountID: "acc1"}
	exampleOrg = cfapi.Zone{ID: "zone2", Name: "example.org", Status: "active", AccountID: "acc1"}
)

// listedZones is a zone cache in which each credential listed the zones given.
func listedZones(byCred map[string][]cfapi.Zone) *zoneCache {
	z := newZoneCache()
	for id, zones := range byCred {
		cz := z.credential(id)
		cz.zones, cz.listed, cz.at = zones, true, t0
	}
	return z
}

// leftOutBy is what the checks of the credentials found: each looked at
// every zone of example.com and example.org but those it left out.
func leftOutBy(byCred map[string][]string) map[string]zoneCheck {
	out := map[string]zoneCheck{}
	for id, zones := range byCred {
		chk := zoneCheck{looked: map[string]bool{"zone1": true, "zone2": true}, excluded: map[string]bool{}, next: t0.Add(recheckFailedEvery)}
		for _, zone := range zones {
			delete(chk.looked, zone)
			chk.excluded[zone] = true
		}
		out[id] = chk
	}
	return out
}

// comView is the view of example.com through the credentials ids, with
// nothing stale or left out, before its state is set.
func comView(ids ...string) ZoneView {
	return ZoneView{Name: "example.com", ID: "zone1", Status: "active", AccountID: "acc1", Credentials: ids, Stale: []string{}, Excluded: []string{}}
}

func TestTheViewOfEachZone(t *testing.T) {
	served := func(v ZoneView, by string) ZoneView {
		v.State, v.ServedBy = ZoneServed, by
		return v
	}
	for _, tt := range []struct {
		name   string
		cache  func() *zoneCache
		ids    []string
		pins   map[string]string
		checks map[string]zoneCheck
		want   []ZoneView
	}{
		{
			name:  "one credential",
			cache: func() *zoneCache { return listedZones(map[string][]cfapi.Zone{"cred1": {exampleCom}}) },
			ids:   []string{"cred1"},
			want:  []ZoneView{served(comView("cred1"), "cred1")},
		},
		{
			name: "two credentials without a pin",
			cache: func() *zoneCache {
				return listedZones(map[string][]cfapi.Zone{"cred1": {exampleCom}, "cred2": {exampleCom}})
			},
			ids: []string{"cred1", "cred2"},
			want: []ZoneView{func() ZoneView {
				v := comView("cred1", "cred2")
				v.State = ZoneFrozen
				v.FrozenWhy = "zone example.com is visible through credentials cred1 and cred2 and none of them served it before"
				return v
			}()},
		},
		{
			name: "two credentials, one of which served it before",
			cache: func() *zoneCache {
				z := listedZones(map[string][]cfapi.Zone{"cred1": {exampleCom}, "cred2": {exampleCom}})
				z.serve("example.com", planner.Zone{ID: "zone1", Name: "example.com", AccountID: "acc1", CredentialID: "cred2"})
				return z
			},
			ids:  []string{"cred1", "cred2"},
			want: []ZoneView{served(comView("cred1", "cred2"), "cred2")},
		},
		{
			name: "two credentials with a pin",
			cache: func() *zoneCache {
				return listedZones(map[string][]cfapi.Zone{"cred1": {exampleCom}, "cred2": {exampleCom}})
			},
			ids:  []string{"cred1", "cred2"},
			pins: map[string]string{"example.com": "cred2"},
			want: []ZoneView{func() ZoneView {
				v := served(comView("cred1", "cred2"), "cred2")
				v.Pinned = "cred2"
				return v
			}()},
		},
		{
			name: "a pin to a credential that leaves the zone out",
			cache: func() *zoneCache {
				return listedZones(map[string][]cfapi.Zone{"cred1": {exampleCom}, "cred2": {exampleCom}})
			},
			ids:    []string{"cred1", "cred2"},
			pins:   map[string]string{"example.com": "cred2"},
			checks: leftOutBy(map[string][]string{"cred1": nil, "cred2": {"zone1"}}),
			want: []ZoneView{func() ZoneView {
				v := comView("cred1", "cred2")
				v.State, v.Pinned, v.Excluded = ZoneNotServed, "cred2", []string{"cred2"}
				return v
			}()},
		},
		{
			name: "every credential leaves the zone out",
			cache: func() *zoneCache {
				return listedZones(map[string][]cfapi.Zone{"cred1": {exampleCom}, "cred2": {exampleCom}})
			},
			ids:    []string{"cred1", "cred2"},
			checks: leftOutBy(map[string][]string{"cred1": {"zone1"}, "cred2": {"zone1"}}),
			want: []ZoneView{func() ZoneView {
				v := comView("cred1", "cred2")
				v.State, v.Excluded = ZoneLeftOut, []string{"cred1", "cred2"}
				return v
			}()},
		},
		{
			name: "a zone that left the listing of its credential",
			cache: func() *zoneCache {
				z := listedZones(map[string][]cfapi.Zone{"cred1": {exampleOrg}})
				z.credential("cred1").stale["example.com"] = exampleCom
				return z
			},
			ids: []string{"cred1"},
			want: []ZoneView{
				func() ZoneView {
					v := comView("cred1")
					v.State, v.Stale = ZoneFrozen, []string{"cred1"}
					v.FrozenWhy = "zone example.com is no longer listed by credential cred1"
					return v
				}(),
				{
					Name: "example.org", ID: "zone2", Status: "active", AccountID: "acc1", State: ZoneServed,
					Credentials: []string{"cred1"}, ServedBy: "cred1", Stale: []string{}, Excluded: []string{},
					FrozenWhy: "zone example.com is no longer listed by credential cred1",
				},
			},
		},
		{
			name:   "a zone the last check did not look at",
			cache:  func() *zoneCache { return listedZones(map[string][]cfapi.Zone{"cred1": {exampleCom}}) },
			ids:    []string{"cred1"},
			checks: map[string]zoneCheck{"cred1": {looked: map[string]bool{}, excluded: map[string]bool{}, next: t0.Add(time.Hour)}},
			want: []ZoneView{func() ZoneView {
				v := comView("cred1")
				v.State = ZoneFrozen
				v.FrozenWhy = "zone example.com is listed by credential cred1, whose last check did not look at it"
				return v
			}()},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			set := tt.cache().set(tt.ids, tt.pins, tt.checks, t0, false)

			require.Equal(t, tt.want, set.views)
		})
	}
}

func TestNoZonesBeforeACredentialListedThem(t *testing.T) {
	require.Equal(t, []ZoneView{}, emptyState().Zones)

	e := newEnv(t)
	e.cf.FailNext("zones", 1, unavailable)
	st := e.cycle()

	require.True(t, hasProblem(st, "its zones are not listed yet"), "%v", st.Problems)
	require.Equal(t, []ZoneView{}, st.Zones)
}

func TestTheZonesOfTheLastCycle(t *testing.T) {
	e, _, _ := servingThrough(t)
	want := []ZoneView{func() ZoneView {
		v := comView("cred1")
		v.State, v.ServedBy = ZoneServed, "cred1"
		return v
	}()}
	require.Equal(t, want, e.eng.State().Zones)

	e.inv.set(incomplete("node pve1 did not answer"))
	e.clock.advance(20 * time.Second)
	st := e.cycle()
	require.NotEmpty(t, st.Hold)
	require.Equal(t, want, st.Zones, "a cycle that holds keeps the zones of the last one")

	require.NoError(t, e.store.DeleteCredential(testCred))
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))
	e.clock.advance(20 * time.Second)
	st = e.cycle()
	require.Equal(t, []ZoneView{}, st.Zones, "without a credential no zone is listed")
}

// The served-zone freeze: the first refusal of the credential that serves a
// zone, the second, and the confirmation that lets the zone go.
func TestTheZonesOfAServedZoneWhoseDNSIsRefused(t *testing.T) {
	e, _ := servingTwoZones(t)
	com := comView("cred1")
	com.State, com.ServedBy = ZoneServed, "cred1"
	org := ZoneView{Name: "example.org", ID: "zone2", Status: "active", AccountID: "acc1", Credentials: []string{"cred1"}, Stale: []string{}, Excluded: []string{}}
	servedOrg := org
	servedOrg.State, servedOrg.ServedBy = ZoneServed, "cred1"
	require.Equal(t, []ZoneView{com, servedOrg}, e.eng.State().Zones)

	e.refuse()
	st := e.cycle()

	once := "the token of credential cred1 could not read the DNS of zone example.org, which it serves"
	require.Equal(t, []string{once + "; account acc1 is left as it is, checking again at 12:15"}, st.Problems)
	frozenCom, frozenOrg := com, org
	frozenCom.FrozenWhy = once
	frozenOrg.State, frozenOrg.Excluded, frozenOrg.FrozenWhy = ZoneFrozen, []string{"cred1"}, once
	require.Equal(t, []ZoneView{frozenCom, frozenOrg}, st.Zones)

	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())
	st = e.cycle()

	require.Equal(t, []string{refusedLost}, st.Problems)
	require.Equal(t, WaitingZone, st.Waiting[0].Kind)
	lost := "credential cred1 can no longer read the DNS of zone example.org, which it serves: grant it Zone > DNS > Edit there"
	frozenCom.FrozenWhy, frozenOrg.FrozenWhy = lost, lost
	require.Equal(t, []ZoneView{frozenCom, frozenOrg}, st.Zones)

	e.apply(true)
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.Empty(t, st.Problems)
	leftOut := org
	leftOut.State, leftOut.Excluded = ZoneLeftOut, []string{"cred1"}
	require.Equal(t, []ZoneView{com, leftOut}, st.Zones, "let go, the zone is no longer served through cred1")
}

func TestACloneOfTheStateOwnsItsZones(t *testing.T) {
	st := populatedState()
	c := st.clone()

	c.Zones[0].Credentials[0] = "changed"
	c.Zones[1].Stale[0] = "changed"
	c.Zones[2].Excluded[0] = "changed"

	require.Equal(t, populatedState(), st)
}
