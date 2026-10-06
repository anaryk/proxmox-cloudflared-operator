package main

import "testing"

func TestTheControlsListenOnLoopbackOnly(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:7071": true,
		"[::1]:7071":     true,
		"0.0.0.0:7071":   false,
		"[::]:7071":      false,
		"10.0.0.5:7071":  false,
		"localhost:7071": false,
		"127.0.0.1":      false,
	} {
		if err := loopback(addr); (err == nil) != ok {
			t.Errorf("loopback(%q) = %v", addr, err)
		}
	}
}
