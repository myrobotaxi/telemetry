package commands

// The MYR-489 in-band virtual-key signal.
//
// Virtual-key pairing happens inside Tesla's own app: our server is never told,
// which is why MYR-448 had to build a polling reconciler instead of an event
// hook. But there IS one signal we already receive and were throwing away — a
// SIGNED vehicle command that Tesla reports as APPLIED. It cannot succeed
// unless the owner's virtual key is enrolled on that car AND the car was
// reachable at that instant, so it is simultaneously proof of pairing and proof
// of wakefulness. Tester Nabil's `adjust_volume` at 21:19Z was exactly that
// proof, and the fleet-config reconciler slept through it.
//
// The seam lives HERE, at the bottom layer, rather than in the one HTTP handler
// that happens to serve owner commands today: every path that reaches a car —
// the owner command endpoint, nav dispatch, anything added later — goes through
// this Executor, so hooking the Executor means no future caller can silently
// fail to report the signal.

// SignedCommandObserver is notified after a signer-required command is
// successfully applied to a vehicle.
//
// UNSIGNED commands are deliberately NOT reported. They are forwarded straight
// to the Fleet REST API and succeed whether or not a virtual key exists, so
// they prove nothing about pairing — reporting them would manufacture the very
// evidence the escalation relies on.
//
// Implementations MUST NOT block. The call happens on the request goroutine
// with the owner waiting on an HTTP response, so anything slower than a channel
// send belongs behind one.
type SignedCommandObserver interface {
	SignedCommandApplied(vin string)
}

// WithSignedCommandObserver registers o to receive applied signed commands.
// A nil observer leaves the Executor unhooked, which is the default and the
// behaviour in every process that does not run the fleet-config reconciler.
func WithSignedCommandObserver(o SignedCommandObserver) Option {
	return func(e *Executor) { e.applied = o }
}

// notifyApplied reports a successfully applied signed command, if anyone is
// listening. Split out so the Executor's hot path reads as one line and the
// nil/unsigned guards live in one place.
func (e *Executor) notifyApplied(cmd Command, vin string) {
	if e.applied == nil || !cmd.SignerRequired {
		return
	}
	e.applied.SignedCommandApplied(vin)
}
