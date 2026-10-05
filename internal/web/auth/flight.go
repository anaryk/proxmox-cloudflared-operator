package auth

import (
	"context"
	"errors"
	"sync"
)

// flight makes one call per key at a time: who asks for a key while its call
// runs waits for that call's answer instead of making another. The first to
// ask makes the call, without the cancellation of its context, so that one
// browser going away does not fail the requests of the others; every other
// stops waiting when its own context ends.
type flight[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*flightCall[V]
}

type flightCall[V any] struct {
	done chan struct{}
	val  V
	err  error
}

var errCallFailed = errors.New("the call this request waited for failed")

func (f *flight[K, V]) do(ctx context.Context, key K, fn func(context.Context) (V, error)) (V, error) {
	f.mu.Lock()
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		select {
		case <-c.done:
			return c.val, c.err
		case <-ctx.Done():
			var zero V
			return zero, ctx.Err()
		}
	}
	if f.calls == nil {
		f.calls = map[K]*flightCall[V]{}
	}
	// The error stays when fn panics: the panic is the caller's, and those
	// who waited learn that the call failed.
	c := &flightCall[V]{done: make(chan struct{}), err: errCallFailed}
	f.calls[key] = c
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.calls, key)
		f.mu.Unlock()
		close(c.done)
	}()
	c.val, c.err = fn(context.WithoutCancel(ctx))
	return c.val, c.err
}
