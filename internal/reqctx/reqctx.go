// Package reqctx carries per-request metadata (request id, client IP and the
// authenticated actor) through context.Context.
package reqctx

import "context"

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyClientIP
	keyActor
)

// Actor types.
const (
	ActorAdmin    = "admin"
	ActorMerchant = "merchant"
	ActorSystem   = "system"
)

// Actor identifies who performed an action.
type Actor struct {
	Type       string // admin, merchant or system
	ID         string // admin token name, API key id, or job name
	MerchantID string // set for merchant actors
}

// System returns an actor for background jobs.
func System(job string) Actor { return Actor{Type: ActorSystem, ID: job} }

// WithRequestID stores the request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

// RequestID returns the request id, or "".
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(keyRequestID).(string)
	return s
}

// WithClientIP stores the client IP.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, keyClientIP, ip)
}

// ClientIP returns the client IP, or "".
func ClientIP(ctx context.Context) string {
	s, _ := ctx.Value(keyClientIP).(string)
	return s
}

// WithActor stores the authenticated actor.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, keyActor, a)
}

// ActorFrom returns the actor, defaulting to an anonymous system actor.
func ActorFrom(ctx context.Context) Actor {
	if a, ok := ctx.Value(keyActor).(Actor); ok {
		return a
	}
	return Actor{Type: ActorSystem, ID: "anonymous"}
}
