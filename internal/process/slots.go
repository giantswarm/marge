package process

import "context"

// Slots bounds how many of one kind of request run at once. A nil Slots
// bounds nothing.
type Slots chan struct{}

// NewSlots returns a bound of n requests at once, at least one.
func NewSlots(n int) Slots {
	return make(Slots, max(n, 1))
}

// acquire takes a slot, or returns ctx's error when the caller gives up
// before one frees. release gives the slot back.
func (s Slots) acquire(ctx context.Context) (release func(), err error) {
	if s == nil {
		return func() {}, nil
	}
	select {
	case s <- struct{}{}:
		return func() { <-s }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
