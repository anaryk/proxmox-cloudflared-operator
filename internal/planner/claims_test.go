package planner

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

const (
	grace   = 60 * time.Second
	primary = "a.example.com"
)

func at(d time.Duration) time.Time { return t0.Add(d) }

// routes builds one route per owner for the hostname. Owners are written as
// "qemu/3", "lxc/5" or "manual/grafana".
func routes(t *testing.T, hostname string, owners ...string) []model.Route {
	t.Helper()
	var out []model.Route
	for _, owner := range owners {
		r := model.Route{
			Hostname: hostname,
			Target:   model.Target{Scheme: model.SchemeHTTP, Port: 80},
		}
		if id, ok := strings.CutPrefix(owner, "manual/"); ok {
			r.Source = model.SourceManual
			r.ManualID = id
		} else {
			guest, err := model.ParseGuestRef(owner)
			require.NoError(t, err)
			r.Source = model.SourceAnnotation
			r.Guest = &guest
		}
		out = append(out, r)
	}
	return out
}

func hostsOf(rs []model.Route) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Hostname)
	}
	return out
}

func ownersOf(rs []model.Route) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Owner())
	}
	return out
}

func waitingOwners(c Claim) []string {
	var out []string
	for _, w := range c.Waiting {
		out = append(out, w.Owner)
	}
	return out
}

// eventKeys lists "kind hostname owner" for every event, in order. It also
// checks that each event explains itself.
func eventKeys(t *testing.T, events []ClaimEvent) []string {
	t.Helper()
	var out []string
	for _, ev := range events {
		require.NotEmpty(t, ev.Detail, "event %s %s %s", ev.Kind, ev.Hostname, ev.Owner)
		out = append(out, string(ev.Kind)+" "+ev.Hostname+" "+ev.Owner)
	}
	return out
}

func holding(owner, identity string, since time.Time, waiting ...Waiter) Claim {
	return Claim{Hostname: primary, Owner: owner, Identity: identity, Since: since, Waiting: waiting}
}

func TestClaimsNewHostname(t *testing.T) {
	in := ClaimInput{
		Routes: append(
			routes(t, primary, "lxc/5", "qemu/20", "qemu/3"),
			routes(t, "b.example.com", "manual/grafana")...,
		),
		Identity: map[string]string{"qemu/3": "id3", "qemu/20": "id20", "lxc/5": "id5"},
		Now:      t0,
		Grace:    grace,
	}

	res := ResolveClaims(in)

	require.Equal(t, []string{primary, "b.example.com"}, hostsOf(res.Winners))
	require.Equal(t, []string{"qemu/3", "manual/grafana"}, ownersOf(res.Winners))
	require.Equal(t, []string{"qemu/20", "lxc/5"}, ownersOf(res.Conflicts))
	require.Equal(t, map[string]Claim{
		primary: holding("qemu/3", "id3", t0, Waiter{"qemu/20", t0}, Waiter{"lxc/5", t0}),
		"b.example.com": {
			Hostname: "b.example.com",
			Owner:    "manual/grafana",
			Since:    t0,
		},
	}, res.Claims)
	require.Equal(t, []string{
		"claimed a.example.com qemu/3",
		"conflict a.example.com qemu/20",
		"conflict a.example.com lxc/5",
		"claimed b.example.com manual/grafana",
	}, eventKeys(t, res.Events))

	t.Run("the next cycle changes nothing", func(t *testing.T) {
		in.Claims = res.Claims
		in.Now = at(time.Minute)

		again := ResolveClaims(in)

		require.Empty(t, again.Events)
		require.Equal(t, res.Claims, again.Claims)
		require.Equal(t, res.Winners, again.Winners)
		require.Equal(t, res.Conflicts, again.Conflicts)
	})
}

func TestClaimsFirstComeDeterministic(t *testing.T) {
	all := routes(t, primary, "lxc/5", "qemu/20", "qemu/3")
	rng := rand.New(rand.NewPCG(7, 11))

	var first ClaimResult
	for i := range 50 {
		shuffled := slices.Clone(all)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })

		res := ResolveClaims(ClaimInput{Routes: shuffled, Now: t0, Grace: grace})

		require.Equal(t, []string{"qemu/3"}, ownersOf(res.Winners), "run %d", i)
		require.Equal(t, []string{"qemu/20", "lxc/5"}, waitingOwners(res.Claims[primary]), "run %d", i)
		if i == 0 {
			first = res
			continue
		}
		require.Equal(t, first, res, "run %d", i)
	}
}

func TestClaimsHolderWinsOverEarlierOwner(t *testing.T) {
	res := ResolveClaims(ClaimInput{
		Routes: routes(t, primary, "qemu/3", "qemu/20", "lxc/5"),
		Claims: map[string]Claim{
			primary: holding("qemu/20", "id20", at(-time.Hour),
				Waiter{"qemu/3", at(-10 * time.Minute)},
				Waiter{"qemu/99", at(-5 * time.Minute)}),
		},
		Identity: map[string]string{"qemu/20": "id20"},
		Now:      t0,
		Grace:    grace,
	})

	require.Equal(t, []string{"qemu/20"}, ownersOf(res.Winners))
	require.Equal(t, []string{"qemu/3", "lxc/5"}, ownersOf(res.Conflicts))
	require.Equal(t, map[string]Claim{
		primary: holding("qemu/20", "id20", at(-time.Hour),
			Waiter{"qemu/3", at(-10 * time.Minute)},
			Waiter{"lxc/5", t0}),
	}, res.Claims)
	require.Equal(t, []string{"conflict a.example.com lxc/5"}, eventKeys(t, res.Events))
}

func TestClaimsHolderBriefAbsenceKeepsHostname(t *testing.T) {
	prior := map[string]Claim{
		primary: holding("qemu/3", "id3", at(-time.Hour), Waiter{"qemu/9", at(-5 * time.Minute)}),
	}
	ids := map[string]string{"qemu/3": "id3", "qemu/9": "id9"}

	// Cycle 1: the holder's route is gone.
	gone := ResolveClaims(ClaimInput{
		Routes:   routes(t, primary, "qemu/9"),
		Claims:   prior,
		Identity: ids,
		Now:      t0,
		Grace:    grace,
	})

	require.Empty(t, gone.Winners)
	require.Equal(t, []string{"qemu/9"}, ownersOf(gone.Conflicts))
	require.Empty(t, gone.Events)
	missing := t0
	require.Equal(t, map[string]Claim{
		primary: {
			Hostname:     primary,
			Owner:        "qemu/3",
			Identity:     "id3",
			Since:        at(-time.Hour),
			MissingSince: &missing,
			Waiting:      []Waiter{{"qemu/9", at(-5 * time.Minute)}},
		},
	}, gone.Claims)

	t.Run("holder returns inside the grace", func(t *testing.T) {
		res := ResolveClaims(ClaimInput{
			Routes:   routes(t, primary, "qemu/3", "qemu/9"),
			Claims:   gone.Claims,
			Identity: ids,
			Now:      at(30 * time.Second),
			Grace:    grace,
		})

		require.Equal(t, []string{"qemu/3"}, ownersOf(res.Winners))
		require.Equal(t, []string{"qemu/9"}, ownersOf(res.Conflicts))
		require.Equal(t, prior, res.Claims)
		require.Nil(t, res.Claims[primary].MissingSince)
		require.Empty(t, res.Events)
	})

	t.Run("holder still absent after the grace", func(t *testing.T) {
		res := ResolveClaims(ClaimInput{
			Routes:   routes(t, primary, "qemu/9"),
			Claims:   gone.Claims,
			Identity: ids,
			Now:      at(61 * time.Second),
			Grace:    grace,
		})

		require.Equal(t, []string{"qemu/9"}, ownersOf(res.Winners))
		require.Empty(t, res.Conflicts)
		require.Equal(t, map[string]Claim{primary: {Hostname: primary, Owner: "qemu/9", Identity: "id9", Since: at(61 * time.Second)}}, res.Claims)
		require.Equal(t, []string{"transferred a.example.com qemu/9"}, eventKeys(t, res.Events))
	})
}

func TestClaimsIdentityChangeNeverTransfers(t *testing.T) {
	in := ClaimInput{
		Routes: routes(t, primary, "qemu/3", "qemu/9"),
		Claims: map[string]Claim{
			primary: holding("qemu/3", "old", at(-time.Hour), Waiter{"qemu/9", at(-5 * time.Minute)}),
		},
		Identity: map[string]string{"qemu/3": "new", "qemu/9": "id9"},
		Now:      t0,
		Grace:    grace,
	}

	res := ResolveClaims(in)

	require.Equal(t, []string{"qemu/3"}, ownersOf(res.Winners))
	require.Equal(t, []string{"qemu/9"}, ownersOf(res.Conflicts))
	require.Equal(t, map[string]Claim{
		primary: holding("qemu/3", "new", at(-time.Hour), Waiter{"qemu/9", at(-5 * time.Minute)}),
	}, res.Claims)
	require.Equal(t, []string{"identity-changed a.example.com qemu/3"}, eventKeys(t, res.Events))

	t.Run("the stored identity is not reported again", func(t *testing.T) {
		in.Claims = res.Claims
		in.Now = at(time.Minute)

		again := ResolveClaims(in)

		require.Empty(t, again.Events)
		require.Equal(t, res.Claims, again.Claims)
	})
}

func TestClaimsManualRouteKeepsEmptyIdentity(t *testing.T) {
	res := ResolveClaims(ClaimInput{
		Routes: routes(t, primary, "manual/grafana"),
		Claims: map[string]Claim{
			primary: {Hostname: primary, Owner: "manual/grafana", Since: at(-time.Hour)},
		},
		Now:   t0,
		Grace: grace,
	})

	require.Equal(t, []string{"manual/grafana"}, ownersOf(res.Winners))
	require.Empty(t, res.Events)
}

func TestClaimsMissingHolderWithinGrace(t *testing.T) {
	since := at(-30 * time.Second)
	claim := holding("qemu/3", "id3", at(-time.Hour), Waiter{"qemu/9", at(-5 * time.Minute)})
	claim.MissingSince = &since

	res := ResolveClaims(ClaimInput{
		Routes: routes(t, primary, "qemu/9", "lxc/5"),
		Claims: map[string]Claim{primary: claim},
		Now:    t0,
		Grace:  grace,
	})

	require.Empty(t, res.Winners)
	require.Equal(t, []string{"qemu/9", "lxc/5"}, ownersOf(res.Conflicts))
	kept := res.Claims[primary]
	require.Equal(t, "qemu/3", kept.Owner)
	require.Equal(t, &since, kept.MissingSince, "the first absence is not forgotten")
	require.Equal(t, []Waiter{{"qemu/9", at(-5 * time.Minute)}, {"lxc/5", t0}}, kept.Waiting)
	require.Equal(t, []string{"conflict a.example.com lxc/5"}, eventKeys(t, res.Events))
}

func TestClaimsTransferGoesToEarliestWaiter(t *testing.T) {
	since := at(-2 * time.Minute)
	claim := holding("qemu/3", "id3", at(-time.Hour),
		Waiter{"qemu/20", at(-20 * time.Minute)},
		Waiter{"qemu/9", at(-5 * time.Minute)})
	claim.MissingSince = &since

	res := ResolveClaims(ClaimInput{
		Routes:   routes(t, primary, "qemu/9", "lxc/5", "qemu/20"),
		Claims:   map[string]Claim{primary: claim},
		Identity: map[string]string{"qemu/20": "id20"},
		Now:      t0,
		Grace:    grace,
	})

	require.Equal(t, []string{"qemu/20"}, ownersOf(res.Winners))
	require.Equal(t, []string{"qemu/9", "lxc/5"}, ownersOf(res.Conflicts))
	require.Equal(t, map[string]Claim{
		primary: holding("qemu/20", "id20", t0, Waiter{"qemu/9", at(-5 * time.Minute)}, Waiter{"lxc/5", t0}),
	}, res.Claims)
	require.Equal(t, []string{
		"conflict a.example.com lxc/5",
		"transferred a.example.com qemu/20",
	}, eventKeys(t, res.Events))
}

func TestClaimsTransferTieGoesToFirstOwner(t *testing.T) {
	since := at(-2 * time.Minute)
	seen := at(-10 * time.Minute)
	claim := holding("qemu/3", "", at(-time.Hour), Waiter{"lxc/5", seen}, Waiter{"qemu/20", seen})
	claim.MissingSince = &since

	res := ResolveClaims(ClaimInput{
		Routes: routes(t, primary, "lxc/5", "qemu/20"),
		Claims: map[string]Claim{primary: claim},
		Now:    t0,
		Grace:  grace,
	})

	require.Equal(t, []string{"qemu/20"}, ownersOf(res.Winners))
	require.Equal(t, []string{"lxc/5"}, waitingOwners(res.Claims[primary]))
	require.Equal(t, []string{"transferred a.example.com qemu/20"}, eventKeys(t, res.Events))
}

func TestClaimsReleaseAfterGrace(t *testing.T) {
	prior := map[string]Claim{primary: holding("qemu/3", "id3", at(-time.Hour))}
	in := ClaimInput{Claims: prior, Now: t0, Grace: grace}

	first := ResolveClaims(in)

	require.Empty(t, first.Winners)
	require.Empty(t, first.Events)
	require.NotNil(t, first.Claims[primary].MissingSince)
	require.Equal(t, t0, *first.Claims[primary].MissingSince)

	in.Claims = first.Claims
	in.Now = at(grace - time.Second)
	inside := ResolveClaims(in)
	require.Empty(t, inside.Events)
	require.Contains(t, inside.Claims, primary)

	in.Now = at(grace)
	after := ResolveClaims(in)
	require.Empty(t, after.Winners)
	require.NotContains(t, after.Claims, primary)
	require.Equal(t, []string{"released a.example.com qemu/3"}, eventKeys(t, after.Events))
}

func TestClaimsZeroGraceActsImmediately(t *testing.T) {
	const other = "b.example.com"
	res := ResolveClaims(ClaimInput{
		Routes: routes(t, primary, "qemu/9"),
		Claims: map[string]Claim{
			primary: holding("qemu/3", "id3", at(-time.Hour), Waiter{"qemu/9", at(-time.Minute)}),
			other:   {Hostname: other, Owner: "manual/old", Since: at(-time.Hour)},
		},
		Now: t0,
	})

	require.Equal(t, []string{"qemu/9"}, ownersOf(res.Winners))
	require.Contains(t, res.Claims, primary)
	require.NotContains(t, res.Claims, other)
	require.Equal(t, []string{
		"transferred a.example.com qemu/9",
		"released b.example.com manual/old",
	}, eventKeys(t, res.Events))
}

func TestClaimsOneOwnerClaimsAHostnameOnce(t *testing.T) {
	two := routes(t, primary, "qemu/3", "qemu/3")
	two[1].Target.Port = 8080

	res := ResolveClaims(ClaimInput{Routes: two, Now: t0, Grace: grace})

	require.Equal(t, []string{"qemu/3"}, ownersOf(res.Winners))
	require.Equal(t, uint16(80), res.Winners[0].Target.Port)
	require.Empty(t, res.Conflicts)
	require.Empty(t, res.Claims[primary].Waiting)
	require.Equal(t, []string{"claimed a.example.com qemu/3"}, eventKeys(t, res.Events))
}

func TestClaimsEventOrder(t *testing.T) {
	const other = "b.example.com"
	since := at(-2 * time.Minute)
	gone := holding("qemu/1", "", at(-time.Hour), Waiter{"qemu/2", at(-time.Minute)})
	gone.MissingSince = &since
	in := ClaimInput{
		Routes: append(
			routes(t, other, "lxc/7", "qemu/6", "qemu/5"),
			routes(t, primary, "qemu/2", "qemu/10")...,
		),
		Claims:   map[string]Claim{primary: gone},
		Identity: map[string]string{"qemu/2": "id2"},
		Now:      t0,
		Grace:    grace,
	}
	want := []string{
		"conflict a.example.com qemu/10",
		"transferred a.example.com qemu/2",
		"claimed b.example.com qemu/5",
		"conflict b.example.com qemu/6",
		"conflict b.example.com lxc/7",
	}

	rng := rand.New(rand.NewPCG(3, 5))
	var first ClaimResult
	for i := range 20 {
		shuffled := in
		shuffled.Routes = slices.Clone(in.Routes)
		rng.Shuffle(len(shuffled.Routes), func(a, b int) {
			shuffled.Routes[a], shuffled.Routes[b] = shuffled.Routes[b], shuffled.Routes[a]
		})

		res := ResolveClaims(shuffled)

		require.Equal(t, want, eventKeys(t, res.Events), "run %d", i)
		require.Equal(t, []string{primary, other}, hostsOf(res.Winners), "run %d", i)
		require.Equal(t, []string{"qemu/10", "qemu/6", "lxc/7"}, ownersOf(res.Conflicts), "run %d", i)
		if i == 0 {
			first = res
			continue
		}
		require.Equal(t, first, res, "run %d", i)
	}
}

func TestClaimsDoesNotAliasInput(t *testing.T) {
	scrambled := func(res ClaimResult) {
		for h, c := range res.Claims {
			for i := range c.Waiting {
				c.Waiting[i].Owner = "mutated"
			}
			if c.MissingSince != nil {
				*c.MissingSince = time.Time{}
			}
			res.Claims[h] = Claim{}
		}
		res.Claims["extra"] = Claim{}
		for _, r := range slices.Concat(res.Winners, res.Conflicts) {
			r.Guest.VMID = 999
		}
	}

	tests := []struct {
		name  string
		input func() ClaimInput
	}{
		{
			name: "holder absent",
			input: func() ClaimInput {
				since := at(-10 * time.Second)
				claim := holding("qemu/3", "id3", at(-time.Hour), Waiter{"qemu/9", at(-time.Minute)})
				claim.MissingSince = &since
				return ClaimInput{
					Routes: routes(t, primary, "qemu/9", "lxc/5"),
					Claims: map[string]Claim{primary: claim},
					Now:    t0,
					Grace:  grace,
				}
			},
		},
		{
			name: "holder present",
			input: func() ClaimInput {
				since := at(-10 * time.Second)
				claim := holding("qemu/3", "id3", at(-time.Hour), Waiter{"qemu/9", at(-time.Minute)})
				claim.MissingSince = &since
				return ClaimInput{
					Routes:   routes(t, primary, "qemu/3", "qemu/9", "lxc/5"),
					Claims:   map[string]Claim{primary: claim},
					Identity: map[string]string{"qemu/3": "id3"},
					Now:      t0,
					Grace:    grace,
				}
			},
		},
		{
			name: "new claim",
			input: func() ClaimInput {
				return ClaimInput{Routes: routes(t, primary, "qemu/3", "qemu/9"), Now: t0, Grace: grace}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.input()
			res := ResolveClaims(in)
			if before, ok := in.Claims[primary]; ok && before.MissingSince != nil && res.Claims[primary].MissingSince != nil {
				require.NotSame(t, before.MissingSince, res.Claims[primary].MissingSince)
			}

			scrambled(res)

			require.Equal(t, tt.input(), in)
		})
	}
}
