package updates

import "context"

type transportDrainKey struct{}

// WithTransportDrain requires cancellation-aware adapters to join their local
// workers before returning. Admission owners can then safely release resources.
func WithTransportDrain(ctx context.Context) context.Context {
	return context.WithValue(ctx, transportDrainKey{}, true)
}

func RequiresTransportDrain(ctx context.Context) bool {
	required, _ := ctx.Value(transportDrainKey{}).(bool)
	return required
}
