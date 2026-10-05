package egress

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var errBoom = errors.New("boom")

func TestSetAppliesOnceForANewSetAndNotForTheSameSetInAnyOrder(t *testing.T) {
	f, n, _, _ := newTestFilter(t)

	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.6:443")))
	require.Len(t, n.applied(), 1)

	require.NoError(t, f.Set(t.Context(), targets("10.0.0.6:443", "10.0.0.5:80")))
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.6:443", "10.0.0.5:80", "10.0.0.5:80")))
	require.Len(t, n.applied(), 1, "the same set, in another order or twice, is no change")
}

func TestSetAppliesWhenTheTargetsChange(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	n.reset()

	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.5:443")))

	require.Len(t, n.applied(), 1)
	require.Contains(t, n.applied()[0], "10.0.0.5 . 80,\n\t\t\t10.0.0.5 . 443\n")
}

func TestSetWithNoTargetsLoadsTheEmptySets(t *testing.T) {
	f, n, _, _ := newTestFilter(t)

	require.NoError(t, f.Set(t.Context(), nil))

	require.Equal(t, []string{Base(testUID, nil, nil)}, n.applied())
}

func TestSetTakesTheZoneAndTheIPv4MappingOffAnAddress(t *testing.T) {
	f, n, _, _ := newTestFilter(t)

	require.NoError(t, f.Set(t.Context(), []Target{
		{Addr: addr("::ffff:10.0.0.5"), Port: 80},
		{Addr: addr("fe80::1%eth0"), Port: 80},
	}))

	require.Contains(t, n.applied()[0], "\t\t\t10.0.0.5 . 80\n")
	require.Contains(t, n.applied()[0], "\t\t\tfe80::1 . 80\n")
	require.NotContains(t, n.applied()[0], "%")
}

func TestSetRefusesAnInvalidTarget(t *testing.T) {
	for name, bad := range map[string]Target{
		"no address": {Port: 80},
		"no port":    {Addr: addr("10.0.0.5")},
	} {
		t.Run(name, func(t *testing.T) {
			f, n, _, _ := newTestFilter(t)

			err := f.Set(t.Context(), append(targets("10.0.0.6:80"), bad))

			require.Error(t, err)
			require.Empty(t, n.applied())
		})
	}
}

func TestSetReadsTheResolversEveryTimeAndAppliesWhenTheyChange(t *testing.T) {
	f, n, r, _ := newTestFilter(t)
	r.set(addr("192.168.1.1"))
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	require.Len(t, n.applied(), 1)
	n.reset()

	r.set(addr("192.168.1.2"), addr("fd00::53"))
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))

	require.Equal(t, 3, r.calls)
	require.Len(t, n.applied(), 1)
	require.Contains(t, n.applied()[0], "\t\t\t192.168.1.2\n")
	require.Contains(t, n.applied()[0], "\t\t\tfd00::53\n")
	require.NotContains(t, n.applied()[0], "192.168.1.1")
}

func TestSetFailsWithoutApplyingWhenTheResolversCannotBeRead(t *testing.T) {
	f, n, r, _ := newTestFilter(t)
	r.err = errBoom

	err := f.Set(t.Context(), targets("10.0.0.5:80"))

	require.ErrorIs(t, err, errBoom)
	require.Empty(t, n.applied())
}

func TestSetReturnsAnApplyErrorAndRetriesNextTime(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	n.apply = func(string) error { return errBoom }

	err := f.Set(t.Context(), targets("10.0.0.5:80"))
	require.ErrorIs(t, err, errBoom)

	n.apply = nil
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	require.Len(t, n.applied(), 2, "the same set is applied again after a failure")
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	require.Len(t, n.applied(), 2)
}

func TestSetAfterAFailureOfAChangedSetRetriesIt(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	n.apply = func(string) error { return errBoom }
	require.Error(t, f.Set(t.Context(), targets("10.0.0.6:80")))
	n.apply = nil
	n.reset()

	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))

	require.Len(t, n.applied(), 1, "the failed apply leaves the old table, which may hold anything the failed one would")
}

func TestSetSubtractsTheBlockedAddressesFromEverySet(t *testing.T) {
	f, n, r, ov := newTestFilter(t)
	r.set(addr("10.0.0.53"), addr("192.168.1.1"))
	_, err := ov.Block(addr("10.0.0.5"))
	require.NoError(t, err)
	_, err = ov.Block(addr("10.0.0.53"))
	require.NoError(t, err)

	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.5:443", "10.0.0.6:80")))

	script := n.applied()[0]
	require.Equal(t, []string{"10.0.0.6 . 80"}, elementsOf(t, script, setTargets4))
	require.Equal(t, []string{"192.168.1.1"}, elementsOf(t, script, setResolvers4))
	require.Equal(t, []string{"10.0.0.5", "10.0.0.53"}, elementsOf(t, script, setBlocked4))
}

func TestSetAppliesWhenTheBlockListChanged(t *testing.T) {
	f, n, _, ov := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.6:80")))
	n.reset()

	_, err := ov.Block(addr("10.0.0.6"))
	require.NoError(t, err)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.6:80")))
	require.Len(t, n.applied(), 1)
	require.Equal(t, []string{"10.0.0.5 . 80"}, elementsOf(t, n.applied()[0], setTargets4))

	_, err = ov.Unblock(addr("10.0.0.6"))
	require.NoError(t, err)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.6:80")))
	require.Len(t, n.applied(), 2)
	require.Contains(t, n.applied()[1], "10.0.0.6 . 80")
}

func TestSetFailsWithoutApplyingWhenTheBlockListCannotBeRead(t *testing.T) {
	f, n, _, ov := newTestFilter(t)
	writeOverride(t, ov, blockedFile, "{not json")

	err := f.Set(t.Context(), targets("10.0.0.5:80"))

	require.ErrorContains(t, err, blockedFile)
	require.Empty(t, n.applied())
}

func TestSetDoesNotLoadTheTableWhileTheFilterIsOff(t *testing.T) {
	f, n, _, ov := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	require.NoError(t, ov.SwitchOff(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)))
	n.reset()

	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.6:80")), "being off is no failure of the cycle")
	require.NoError(t, f.Remove(t.Context(), addr("10.0.0.5")))
	require.Empty(t, n.applied())

	_, err := ov.SwitchOn()
	require.NoError(t, err)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.6:80")))
	require.Len(t, n.applied(), 1, "the table the switch removed is loaded again")
	require.Contains(t, n.applied()[0], "10.0.0.6 . 80")
}

func TestSetWithTheSameTargetsAfterTheSwitchWasOffAppliesAgain(t *testing.T) {
	f, n, _, ov := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	require.NoError(t, ov.SwitchOff(time.Now()))
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	_, err := ov.SwitchOn()
	require.NoError(t, err)
	n.reset()

	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))

	require.Len(t, n.applied(), 1)
}

func TestAFilterForRootIsRefused(t *testing.T) {
	n := &fakeNft{}
	f := New(n, 0, (&resolvers{}).get, NewOverrides(t.TempDir()))

	require.ErrorContains(t, f.Set(t.Context(), targets("10.0.0.5:80")), "root")
	require.Empty(t, n.applied())
}

func TestRemoveTakesTheAddressOutAtOnce(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.5:443", "10.0.0.6:80")))
	n.reset()

	require.NoError(t, f.Remove(t.Context(), addr("10.0.0.5")))

	require.Equal(t, []string{"delete element inet pco_egress targets4 { 10.0.0.5 . 80, 10.0.0.5 . 443 }\n"}, n.applied())
}

func TestRemoveOfAnAddressTheTableDoesNotHoldAppliesNothing(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.6:80")))
	n.reset()

	require.NoError(t, f.Remove(t.Context(), addr("10.0.0.5")))

	require.Empty(t, n.applied())
}

func TestRemoveLeavesAResolverWithTheSameAddress(t *testing.T) {
	f, n, r, _ := newTestFilter(t)
	r.set(addr("10.0.0.5"))
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	n.reset()

	require.NoError(t, f.Remove(t.Context(), addr("10.0.0.5")))

	require.Equal(t, []string{"delete element inet pco_egress targets4 { 10.0.0.5 . 80 }\n"}, n.applied())
}

func TestTheNextSetAfterRemoveAppliesTheSetItIsGiven(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.6:80")))
	require.NoError(t, f.Remove(t.Context(), addr("10.0.0.5")))
	n.reset()

	require.NoError(t, f.Set(t.Context(), targets("10.0.0.6:80")))
	require.Empty(t, n.applied(), "the table already holds what the cycle wants")

	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.6:80")))
	require.Len(t, n.applied(), 1, "a target that verified again comes back")
}

func TestRemoveKeepsTheBlockedAddresses(t *testing.T) {
	f, n, _, ov := newTestFilter(t)
	_, err := ov.Block(addr("10.0.0.9"))
	require.NoError(t, err)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.6:80")))
	require.NoError(t, f.Remove(t.Context(), addr("10.0.0.5")))
	n.reset()

	require.NoError(t, f.Set(t.Context(), targets("10.0.0.6:80")))

	require.Empty(t, n.applied(), "the table already holds what the cycle wants, blocked set included")
}

func TestRemoveReplacesTheWholeTableWhenTheDeleteFails(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.6:80")))
	n.apply = func(script string) error {
		if strings.HasPrefix(script, "delete element") {
			return errBoom
		}
		return nil
	}
	n.reset()

	require.NoError(t, f.Remove(t.Context(), addr("10.0.0.5")))

	require.Len(t, n.applied(), 2)
	require.Equal(t, render(testUID, contents{targets: targets("10.0.0.6:80")}), n.applied()[1])
}

func TestRemoveReturnsTheErrorWhenNothingCouldBeApplied(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.6:80")))
	n.apply = func(string) error { return errBoom }

	require.ErrorIs(t, f.Remove(t.Context(), addr("10.0.0.5")), errBoom)

	n.apply = nil
	n.reset()
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.6:80")))
	require.Len(t, n.applied(), 1, "the next Set applies whatever the last one held")
}

// A daemon that starts finds the table it loaded before, or the one of the
// boot unit: until its first Set, the filter knows no targets, and taking one
// address out must not take the others with it.
func TestRemoveBeforeAnySetTakesTheAddressOutOfTheTableAsItIs(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	n.setLive(realListing(t, "1.1.3").bytes(t))

	require.NoError(t, f.Remove(t.Context(), addr("10.0.0.5")))

	require.Equal(t, []string{"delete element inet pco_egress targets4 { 10.0.0.5 . 80, 10.0.0.5 . 8080 }\n"}, n.applied())

	t.Run("and the next Set loads the whole table", func(t *testing.T) {
		n.reset()
		require.NoError(t, f.Set(t.Context(), targets("10.0.0.6:443")))
		require.Equal(t, []string{render(testUID, contents{targets: targets("10.0.0.6:443")})}, n.applied())
	})
}

func TestRemoveBeforeAnySetOfAnAddressTheTableDoesNotHold(t *testing.T) {
	for name, live := range map[string]func(*fakeNft){
		"not loaded":        func(n *fakeNft) { n.listErr = ErrNotLoaded },
		"another address":   func(n *fakeNft) { n.setLive(realListing(t, "1.1.3").bytes(t)) },
		"no targets at all": func(n *fakeNft) { n.setLive(realListing(t, "1.1.3").with(t, nil, nil).bytes(t)) },
	} {
		t.Run(name, func(t *testing.T) {
			f, n, _, _ := newTestFilter(t)
			live(n)

			require.NoError(t, f.Remove(t.Context(), addr("10.0.0.99")))

			require.Empty(t, n.applied())
		})
	}
}

func TestRemoveBeforeAnySetReturnsAListingItCannotRead(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	n.setLive([]byte("{"))

	require.ErrorIs(t, f.Remove(t.Context(), addr("10.0.0.5")), ErrUnreadable)
	require.Empty(t, n.applied(), "nothing is loaded in the dark")
}

func TestVerifyAcceptsTheTableLastApplied(t *testing.T) {
	for _, version := range []string{"1.0.6", "1.1.3"} {
		t.Run(version, func(t *testing.T) {
			f, n, r, _ := newTestFilter(t)
			r.set(addr("192.168.1.1"))
			tg := targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443")
			require.NoError(t, f.Set(t.Context(), tg))
			n.setLive(realListing(t, version).bytes(t))

			require.NoError(t, f.Verify(t.Context()))
		})
	}
}

func TestVerifyReportsATableThatIsGone(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	n.listErr = ErrNotLoaded

	err := f.Verify(t.Context())

	require.ErrorIs(t, err, ErrChanged)
	require.ErrorIs(t, err, ErrNotLoaded)
}

func TestVerifyReportsWhatDiffers(t *testing.T) {
	tg := targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443")
	rs := []netip.Addr{addr("192.168.1.1")}
	tests := []struct {
		name string
		edit func(t *testing.T, l listing) listing
		want string
	}{
		{"a flushed table", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				var out listing
				for _, e := range l {
					if _, rule := e["rule"]; !rule {
						out = append(out, e)
					}
				}
				return out
			}).with(t, nil, nil)
		}, "chain output has 0 rules, want 1"},
		{"a rule added at the top", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				first := l.rules(chainConnector)[0]
				added := map[string]any{"rule": map[string]any{
					"family": "inet", "table": tableName, "chain": chainConnector, "handle": 99,
					"expr": []any{map[string]any{"accept": nil}},
				}}
				return append(l[:first], append(listing{added}, l[first:]...)...)
			})
		}, "chain connector has 20 rules, want 19"},
		{"a rule changed", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				last := l.rules(chainConnector)
				l[last[len(last)-1]]["rule"].(map[string]any)["expr"] = []any{map[string]any{"accept": nil}}
				return l
			})
		}, "chain connector: rule 19 differs"},
		{"two rules swapped", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				r := l.rules(chainConnector)
				l[r[2]], l[r[3]] = l[r[3]], l[r[2]]
				return l
			})
		}, "chain connector: rule 3 differs"},
		{"another uid", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				expr := l[l.rules(chainOutput)[0]]["rule"].(map[string]any)["expr"].([]any)
				expr[0].(map[string]any)["match"].(map[string]any)["right"] = 0
				return l
			})
		}, "chain output: rule 1 differs"},
		{"policy drop", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				l.object(t, "chain", chainOutput)["policy"] = "drop"
				return l
			})
		}, "chain output is"},
		{"another hook", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				l.object(t, "chain", chainOutput)["hook"] = "input"
				return l
			})
		}, "chain output is filter hook input priority -10 policy accept"},
		{"the exclusions of the edge turned into the only destinations", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				for _, i := range l.rules(chainConnector) {
					expr := l[i]["rule"].(map[string]any)["expr"].([]any)
					if m, ok := expr[0].(map[string]any)["match"].(map[string]any); ok && m["op"] == "!=" {
						m["op"] = "=="
					}
				}
				return l
			})
		}, "chain connector: rule 15 differs"},
		{"the statements of a rule in another order", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				r := l.rules(chainConnector)
				expr := l[r[len(r)-1]]["rule"].(map[string]any)["expr"].([]any)
				expr[0], expr[1] = expr[1], expr[0]
				return l
			})
		}, "chain connector: rule 19 differs"},
		{"another priority", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				l.object(t, "chain", chainOutput)["prio"] = 10
				return l
			})
		}, "chain output is"},
		{"the connector chain hooked", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				c := l.object(t, "chain", chainConnector)
				c["type"], c["hook"], c["prio"], c["policy"] = "filter", "output", 0, "accept"
				return l
			})
		}, "chain connector is"},
		{"no set of the targets of allowNode", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				return slices.DeleteFunc(l, func(e map[string]any) bool {
					s, ok := e["set"].(map[string]any)
					return ok && s["name"] == setAllowNode6
				})
			})
		}, "no set allownode6"},
		{"a set of the targets of allowNode of another type", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				l.object(t, "set", setAllowNode4)["type"] = "ipv4_addr"
				return l
			})
		}, `set allownode4 is "ipv4_addr", want ["ipv4_addr","inet_service"]`},
		{"a missing element", func(t *testing.T, l listing) listing {
			return l.with(t, tg[1:], rs)
		}, "set targets4 lacks 10.0.0.5:80"},
		{"an extra element", func(t *testing.T, l listing) listing {
			return l.with(t, append(targets("10.0.0.7:22"), tg...), rs)
		}, "set targets4 holds 10.0.0.7:22"},
		{"an extra IPv6 target", func(t *testing.T, l listing) listing {
			return l.with(t, append(targets("[fd00::7]:22"), tg...), rs)
		}, "set targets6 holds [fd00::7]:22"},
		{"a missing resolver", func(t *testing.T, l listing) listing {
			return l.with(t, tg, nil)
		}, "set resolvers4 lacks 192.168.1.1"},
		{"an extra resolver", func(t *testing.T, l listing) listing {
			return l.with(t, tg, append(rs, addr("fd00::53")))
		}, "set resolvers6 holds fd00::53"},
		{"an extra blocked address", func(t *testing.T, l listing) listing {
			return l.withBlocked(t, addr("fd00::99"))
		}, "set blocked6 holds fd00::99"},
		{"an element pco cannot read", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				l.object(t, "set", setTargets4)["elem"] = []any{map[string]any{"range": []any{"10.0.0.0", "10.0.0.9"}}}
				return l
			})
		}, "set targets4 holds an element pco cannot read"},
		{"a set of another type", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				l.object(t, "set", setResolvers4)["type"] = "ipv6_addr"
				return l
			})
		}, "set resolvers4 is"},
		{"a set with flags", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				l.object(t, "set", setTargets4)["flags"] = []any{"interval"}
				return l
			})
		}, "set targets4 is"},
		{"a missing set", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				return drop(l, "set", setResolvers6)
			})
		}, "no set resolvers6"},
		{"a missing counter", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				return drop(l, "counter", counterLocal)
			})
		}, "no counter rejected_local"},
		{"another chain", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				return append(l, map[string]any{"chain": map[string]any{
					"family": "inet", "table": tableName, "name": "extra", "handle": 50,
					"type": "filter", "hook": "output", "prio": -300, "policy": "accept",
				}})
			})
		}, "an unexpected chain extra"},
		{"another set", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				return append(l, map[string]any{"set": map[string]any{
					"family": "inet", "table": tableName, "name": "extra", "handle": 50, "type": "ipv4_addr",
				}})
			})
		}, "an unexpected set extra"},
		{"another kind of object", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				return append(l, map[string]any{"quota": map[string]any{
					"family": "inet", "table": tableName, "name": "q", "handle": 50, "bytes": 1,
				}})
			})
		}, "an unexpected quota"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, n, r, _ := newTestFilter(t)
			r.set(rs...)
			require.NoError(t, f.Set(t.Context(), tg))
			n.setLive(tc.edit(t, realListing(t, "1.1.3")).bytes(t))

			err := f.Verify(t.Context())

			require.ErrorIs(t, err, ErrChanged)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// drop returns the listing without the object of a kind and name.
func drop(l listing, kind, name string) listing {
	var out listing
	for _, e := range l {
		if o, ok := e[kind].(map[string]any); ok && o["name"] == name {
			continue
		}
		out = append(out, e)
	}
	return out
}

func TestVerifyDoesNotTakeOtherWordsForAChange(t *testing.T) {
	tg := targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443")
	rs := []netip.Addr{addr("192.168.1.1")}
	tests := []struct {
		name string
		edit func(t *testing.T, l listing) listing
	}{
		{"counters that counted", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				l.object(t, "counter", counterOther)["packets"] = 12
				l.object(t, "counter", counterOther)["bytes"] = 720
				return l
			})
		}},
		{"other handles", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				for i, e := range l {
					for _, o := range e {
						if m, ok := o.(map[string]any); ok {
							if _, has := m["handle"]; has {
								m["handle"] = 1000 + i
							}
						}
					}
				}
				return l
			})
		}},
		{"objects in another order", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				var rules, others listing
				for _, e := range l {
					if _, ok := e["rule"]; ok {
						rules = append(rules, e)
					} else {
						others = append([]map[string]any{e}, others...)
					}
				}
				return append(others, rules...)
			})
		}},
		{"elements in another order", func(t *testing.T, l listing) listing {
			return l.with(t, []Target{tg[2], tg[0], tg[1]}, rs)
		}},
		{"protocols by name", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				replaceAll(l, map[string]any{"set": []any{6, 17}}, map[string]any{"set": []any{"udp", "tcp"}})
				return l
			})
		}},
		{"a single flag as a string", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				replaceAll(l, map[string]any{"result": "type", "flags": []any{"daddr"}}, map[string]any{"result": "type", "flags": "daddr"})
				return l
			})
		}},
		{"an older form of the state match", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				replaceAll(l, map[string]any{"op": "in", "left": map[string]any{"ct": map[string]any{"key": "state"}}, "right": "invalid"},
					map[string]any{"op": "==", "left": map[string]any{"ct": map[string]any{"key": "state"}}, "right": []any{"invalid"}})
				return l
			})
		}},
		{"a comment on a rule", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				l[l.rules(chainConnector)[0]]["rule"].(map[string]any)["comment"] = "kept"
				return l
			})
		}},
		{"elements with their own fields", func(t *testing.T, l listing) listing {
			return l.edit(t, func(l listing) listing {
				s := l.object(t, "set", setResolvers4)
				s["elem"] = []any{map[string]any{"elem": map[string]any{"val": "192.168.1.1", "comment": "dns"}}}
				return l
			})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, n, r, _ := newTestFilter(t)
			r.set(rs...)
			require.NoError(t, f.Set(t.Context(), tg))
			n.setLive(tc.edit(t, realListing(t, "1.1.3")).bytes(t))

			require.NoError(t, f.Verify(t.Context()))
		})
	}
}

// replaceAll replaces every value in the listing that equals old, compared as
// JSON, with replacement.
func replaceAll(l listing, old, replacement any) {
	for _, e := range l {
		for k, v := range e {
			e[k] = replaced(v, old, replacement)
		}
	}
}

func replaced(v, old, replacement any) any {
	if jsonEqual(v, old) {
		return replacement
	}
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = replaced(e, old, replacement)
		}
	case []any:
		for i, e := range x {
			x[i] = replaced(e, old, replacement)
		}
	}
	return v
}

func TestVerifyWhileTheFilterIsOffSaysSoAndNothingElse(t *testing.T) {
	f, n, _, ov := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	require.NoError(t, ov.SwitchOff(time.Now()))
	n.listErr = ErrNotLoaded

	err := f.Verify(t.Context())

	require.ErrorIs(t, err, ErrOff)
	require.NotErrorIs(t, err, ErrChanged)
}

func TestVerifyAfterADifferenceMakesTheNextSetApply(t *testing.T) {
	tg := targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443")
	for name, live := range map[string]func(t *testing.T, n *fakeNft){
		"a table that is gone": func(_ *testing.T, n *fakeNft) { n.listErr = ErrNotLoaded },
		"a table that differs": func(t *testing.T, n *fakeNft) {
			n.setLive(realListing(t, "1.1.3").with(t, tg[1:], []netip.Addr{addr("192.168.1.1")}).bytes(t))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, n, r, _ := newTestFilter(t)
			r.set(addr("192.168.1.1"))
			require.NoError(t, f.Set(t.Context(), tg))
			live(t, n)
			require.ErrorIs(t, f.Verify(t.Context()), ErrChanged)
			n.reset()

			require.NoError(t, f.Set(t.Context(), tg))

			require.Len(t, n.applied(), 1, "the same set is applied again")
			n.setLive(realListing(t, "1.1.3").bytes(t))
			require.NoError(t, f.Verify(t.Context()))
			require.NoError(t, f.Set(t.Context(), tg))
			require.Len(t, n.applied(), 1)
		})
	}
}

func TestVerifyAfterRemoveComparesTheCompleteTable(t *testing.T) {
	f, n, r, _ := newTestFilter(t)
	r.set(addr("192.168.1.1"))
	tg := targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443", "10.0.0.7:22")
	require.NoError(t, f.Set(t.Context(), tg))
	require.NoError(t, f.Remove(t.Context(), addr("10.0.0.7")))

	n.setLive(realListing(t, "1.1.3").bytes(t))
	require.NoError(t, f.Verify(t.Context()))

	n.setLive(realListing(t, "1.1.3").with(t, tg, []netip.Addr{addr("192.168.1.1")}).bytes(t))
	require.ErrorContains(t, f.Verify(t.Context()), "set targets4 holds 10.0.0.7:22")
}

func TestVerifyTakesTheBlockedAddressesOutOfWhatItExpects(t *testing.T) {
	f, n, r, ov := newTestFilter(t)
	r.set(addr("192.168.1.1"))
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443", "10.0.0.7:22")))
	// What pco egress block does: the list, the elements out of the live
	// table and the address into its blocked set.
	_, err := ov.Block(addr("10.0.0.7"))
	require.NoError(t, err)
	n.setLive(realListing(t, "1.1.3").withBlocked(t, addr("10.0.0.7")).bytes(t))

	require.NoError(t, f.Verify(t.Context()))

	n.setLive(realListing(t, "1.1.3").bytes(t))
	require.ErrorContains(t, f.Verify(t.Context()), "set blocked4 lacks 10.0.0.7")
}

func TestSetPutsTheBlockedAddressesIntoTheirSets(t *testing.T) {
	f, n, _, ov := newTestFilter(t)
	_, err := ov.Block(addr("10.0.0.7"))
	require.NoError(t, err)
	_, err = ov.Block(addr("fd00::7"))
	require.NoError(t, err)

	require.NoError(t, f.Set(t.Context(), targets("10.0.0.7:22", "10.0.0.8:22")))

	require.Equal(t, []string{"10.0.0.7"}, elementsOf(t, n.applied()[0], setBlocked4))
	require.Equal(t, []string{"fd00::7"}, elementsOf(t, n.applied()[0], setBlocked6))
	require.Equal(t, []string{"10.0.0.8 . 22"}, elementsOf(t, n.applied()[0], setTargets4))
}

func TestVerifyBeforeAnythingWasAppliedComparesAllButTheElements(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	n.setLive(realListing(t, "1.0.6").bytes(t))

	require.NoError(t, f.Verify(t.Context()))
	require.Empty(t, n.applied())

	n.setLive(realListing(t, "1.0.6").edit(t, func(l listing) listing {
		l.object(t, "chain", chainOutput)["policy"] = "drop"
		return l
	}).bytes(t))
	require.ErrorIs(t, f.Verify(t.Context()), ErrChanged)
}

func TestVerifyReturnsAFailureToListAsItIs(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	n.listErr = errBoom

	err := f.Verify(t.Context())

	require.ErrorIs(t, err, errBoom)
	require.NotErrorIs(t, err, ErrChanged)
}

// dormant returns the listing with flags on its table, written as an nft of
// the given version writes the dormant flag: 1.0.6 prints it wrongly.
func dormant(t *testing.T, version string) listing {
	t.Helper()
	spelling := map[string]any{"1.1.3": "dormant", "1.0.6": "rejected_local", "list": []any{"dormant"}}[version]
	listed := version
	if version == "list" {
		listed = "1.1.3"
	}
	return realListing(t, listed).edit(t, func(l listing) listing {
		for _, e := range l {
			if table, ok := e["table"].(map[string]any); ok {
				table["flags"] = spelling
			}
		}
		return l
	})
}

func TestVerifyReportsADormantTable(t *testing.T) {
	for _, version := range []string{"1.1.3", "1.0.6", "list"} {
		t.Run(version, func(t *testing.T) {
			f, n, r, _ := newTestFilter(t)
			r.set(addr("192.168.1.1"))
			require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443")))
			n.setLive(dormant(t, version).bytes(t))

			err := f.Verify(t.Context())

			require.ErrorIs(t, err, ErrChanged)
			require.ErrorContains(t, err, "the table has flags")
		})
	}
}

func TestVerifyTakesAListingItCannotReadForAChange(t *testing.T) {
	// nft 1.0.6 can print a dormant table as a listing that is no JSON.
	for name, raw := range map[string]string{
		"cut short":   `{"nftables": [{"table": {"family": "inet", "name": "pco_egress", "handle": 1, "flags": `,
		"not json":    "Error: something",
		"no nftables": `{"other": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			f, n, _, _ := newTestFilter(t)
			require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
			n.setLive([]byte(raw))

			err := f.Verify(t.Context())

			require.ErrorIs(t, err, ErrChanged)
			require.ErrorContains(t, err, "cannot be read")
			n.reset()
			require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
			require.Len(t, n.applied(), 1, "the table is applied again")
		})
	}
}

func TestTheComparatorsOrderTargetsByAddressAndPort(t *testing.T) {
	at := func(s string, allowNode bool) Target {
		ap := netip.MustParseAddrPort(s)
		return Target{Addr: ap.Addr(), Port: ap.Port(), AllowNode: allowNode}
	}
	for _, tt := range []struct {
		name                      string
		a, b                      Target
		endpoints, allowNodeFirst int
	}{
		{"the same", at("10.0.0.5:80", false), at("10.0.0.5:80", false), 0, 0},
		{"another address", at("10.0.0.5:80", false), at("10.0.0.6:80", false), -1, -1},
		{"another port", at("10.0.0.5:443", true), at("10.0.0.5:80", false), 1, 1},
		{"allowNode against none", at("10.0.0.5:80", true), at("10.0.0.5:80", false), 0, -1},
		{"none against allowNode", at("10.0.0.5:80", false), at("10.0.0.5:80", true), 0, 1},
		{"the address before the mark", at("10.0.0.6:80", true), at("10.0.0.5:80", false), 1, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.endpoints, CompareEndpoints(tt.a, tt.b))
			require.Equal(t, tt.allowNodeFirst, CompareAllowNodeFirst(tt.a, tt.b))
			require.Equal(t, -tt.endpoints, CompareEndpoints(tt.b, tt.a))
			require.Equal(t, -tt.allowNodeFirst, CompareAllowNodeFirst(tt.b, tt.a))
		})
	}
}
