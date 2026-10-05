package engine

import "context"

type actorKey struct{}

// WithActor returns ctx carrying who asks for what is done with it, as the
// events of an admin action record it.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorOf is the actor ctx carries, or empty.
func ActorOf(ctx context.Context) string {
	actor, _ := ctx.Value(actorKey{}).(string)
	return actor
}
