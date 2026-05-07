package cimstomp

// This file holds the package seam used only by the cimstomptest
// subpackage. The names are exported because Go requires it for
// cross-package access; the comments and naming are the discipline.
//
// Do not call these from production code. Use cimstomp.Client.Subscribe
// instead. They exist solely so cimstomptest can hand out a
// *Subscription without dialing a broker.
//
// The internal/ ancestor blocks cross-repo imports, so the worst-case
// blast radius of these symbols is this module's own callers.

// NewTestSubscription constructs a *Subscription whose Messages channel
// is the returned msgs (typed bidirectional inside cimstomp; narrowed to
// chan<- Message by cimstomptest). For cimstomptest use only.
func NewTestSubscription() (*Subscription, chan Message) {
	msgs := make(chan Message, 8)
	s := &Subscription{msgs: msgs}
	return s, msgs
}

// SetTestErr records the end-cause on a Subscription built via
// NewTestSubscription. For cimstomptest use only.
func SetTestErr(s *Subscription, err error) { s.setErr(err) }
