package gridappsdclient

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/GRIDAPPSD/gridappsd-go/fieldbus"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
)

// The bridge's STOMP subscriptions died roughly 15 seconds
// after startup and never recovered, while the process stayed alive and
// kept serving HTTP. The chain, end to end:
//
//  1. cimstomp and gridappsd-go both dial with a symmetric 10s STOMP
//     heartbeat (internal/stomp.DefaultHeartBeat). go-stomp then arms a
//     read timer of max(server-offered, requested) plus its 5s
//     DefaultHeartBeatError (conn.go:212-223), so the connection has a
//     hard 15s "nothing arrived" deadline. That is the observed 15s to
//     the second, not a coincidence.
//  2. When the broker sends nothing at all for that window (not even a
//     heartbeat newline), go-stomp's processLoop synthesises an ERROR
//     frame carrying "read timeout" (conn.go:327-331), fans it out to
//     every subscription channel, and MustDisconnects the socket. The
//     WHOLE connection dies, not one subscription: the publish side of
//     the same bus is dead from that instant too.
//  3. gridappsd-go's transport bridge forwards that as
//     transport.Msg{Err} and exits (internal/stomp/stomp.go:167,183).
//     router.readLoop treats it as terminal, pushes the error into an
//     errSink nobody reads (internal/router/router.go:186-195, an
//     upstream gap), and returns.
//  4. Nothing above the router ever learns. Subscriber.relay only ends
//     on ctx cancel, so the consumer's Messages channel stays open and
//     its range loop blocks forever on a channel that will never carry
//     another frame. Silent, permanent deafness.
//
// Supervisor closes step 4. It cannot make step 1 not happen from this
// repo (the heartbeat request is gridappsd-go's and go-stomp arms the
// timer even when the broker answers heart-beat:0,0), so it does the
// next best thing: it notices the dead connection, says so at ERROR,
// reconnects the bus (which re-runs the two-step GOSS token bootstrap
// via gridappsd.Connect: reused, not reimplemented), and RESUBSCRIBES
// every destination it owns. Reconnecting without resubscribing would
// leave a live socket receiving nothing, which looks healthier while
// being just as deaf.

const (
	// DefaultProbeInterval is how often Supervisor checks that the bus
	// connection is still usable. Well under the 15s go-stomp read
	// deadline that kills it, so at most one probe interval of frames is
	// lost on top of the deadline itself.
	DefaultProbeInterval = 5 * time.Second

	// DefaultProbeTimeout bounds a single liveness probe. A probe that
	// has not returned within this window is treated as a failure: the
	// bus is not answering, which is exactly the condition being probed
	// for. Fail closed.
	DefaultProbeTimeout = 10 * time.Second

	// DefaultMaxRecoverAttempts bounds consecutive reconnect attempts
	// before Supervisor gives up and fails every subscription. See
	// recover for why exhaustion is fatal rather than silently degraded.
	DefaultMaxRecoverAttempts = 10

	// recoverBackoffBase and recoverBackoffMax bound the exponential
	// backoff between reconnect attempts.
	recoverBackoffBase = 500 * time.Millisecond
	recoverBackoffMax  = 10 * time.Second
)

// ErrBusUnrecovered is the sentinel wrapped into every Subscription.Err
// when Supervisor exhausts its reconnect attempts. Exported so a caller
// can errors.Is a subscription failure back to "the bus never came
// back" and distinguish it from a graceful ctx-driven shutdown.
var ErrBusUnrecovered = errors.New("gridappsdclient: bus did not recover")

// Bus is the subset of fieldbus.MessageBus that Supervisor needs.
// Declared here (rather than taking fieldbus.MessageBus directly) so a
// test can drive the reconnect and resubscribe paths without a broker.
type Bus interface {
	Connect(ctx context.Context) error
	Disconnect() error
	Subscribe(ctx context.Context, destination string, h fieldbus.Handler) (fieldbus.Token, error)
	Unsubscribe(ctx context.Context, destination string, tok fieldbus.Token) error
}

// compile-time assertion: a fieldbus.MessageBus must satisfy Bus.
var _ Bus = (fieldbus.MessageBus)(nil)

// SupervisorOption configures optional Supervisor behavior.
type SupervisorOption func(*Supervisor)

// WithProbeDestination sets the destination used for the liveness
// probe. It MUST be a destination this bridge is already entitled to
// subscribe to: a destination the broker's ACL rejects makes the broker
// answer with an ERROR frame, which go-stomp handles by closing the
// connection (conn.go:376-388), turning the health check itself into an
// outage. cmd/bridge passes the per-simulation log topic, a sibling of
// the output and input topics the bridge already subscribes to, so if
// it were denied the bridge's real subscriptions would be denied too.
//
// With no probe destination set, Supervisor relays messages but cannot
// detect a dead connection; it logs that at startup rather than
// pretending to supervise.
func WithProbeDestination(dest string) SupervisorOption {
	return func(s *Supervisor) { s.probeDest = dest }
}

// WithProbeInterval overrides DefaultProbeInterval.
func WithProbeInterval(d time.Duration) SupervisorOption {
	return func(s *Supervisor) { s.probeInterval = d }
}

// WithProbeTimeout overrides DefaultProbeTimeout.
func WithProbeTimeout(d time.Duration) SupervisorOption {
	return func(s *Supervisor) { s.probeTimeout = d }
}

// WithMaxRecoverAttempts overrides DefaultMaxRecoverAttempts.
func WithMaxRecoverAttempts(n int) SupervisorOption {
	return func(s *Supervisor) { s.maxRecoverAttempts = n }
}

// supervisedSub is one destination Supervisor owns: the caller-facing
// subscription handle, the handler registered on the bus (re-registered
// verbatim on every reconnect, which is what makes the consumer's
// Messages channel survive a reconnect), and the current bus token.
type supervisedSub struct {
	dest    string
	handler fieldbus.Handler
	sub     *subscription

	// stop is closed by fail to end the relay for a reason OTHER than
	// ctx cancel (a permanently unrecoverable bus). Guarded by stopOnce
	// so a double fail cannot close it twice.
	stop     chan struct{}
	stopOnce sync.Once

	// tok is the current bus token for dest. Rewritten on every
	// successful resubscribe; guarded by Supervisor.mu.
	tok fieldbus.Token
}

// Supervisor is a health-supervised sim.SubscribeClient over a
// fieldbus.MessageBus. It relays frames exactly as Subscriber does, and
// additionally: probes the bus for liveness, reconnects it when the
// probe fails, and resubscribes every destination it owns onto the new
// connection.
//
// Supervisor deliberately reconnects the SHARED bus rather than a
// private connection of its own. The go-stomp read timeout kills the
// whole connection, so the bridge's publish path is dead from the same
// instant as its subscriptions; healing only a private subscribe
// connection would leave the bridge silently unable to publish. Nothing
// here changes what the publish path sends or which destinations it
// sends to: reconnecting restores the existing publish path, it does
// not redirect it.
type Supervisor struct {
	bus Bus

	probeDest          string
	probeInterval      time.Duration
	probeTimeout       time.Duration
	maxRecoverAttempts int

	// probe is the liveness check, a seam for tests. Defaults to
	// probeBus.
	probe func(context.Context) error

	// sleep waits d or until ctx is done, reporting true if it waited
	// the full duration. A seam so tests do not pay real backoff.
	sleep func(ctx context.Context, d time.Duration) bool

	mu           sync.Mutex
	subs         []*supervisedSub
	watchStarted bool
}

// NewSupervisor returns a Supervisor over bus. bus must already be
// connected; NewSupervisor performs no I/O.
func NewSupervisor(bus Bus, opts ...SupervisorOption) *Supervisor {
	s := &Supervisor{
		bus:                bus,
		probeInterval:      DefaultProbeInterval,
		probeTimeout:       DefaultProbeTimeout,
		maxRecoverAttempts: DefaultMaxRecoverAttempts,
		sleep:              sleepCtx,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.probe == nil {
		s.probe = s.probeBus
	}
	return s
}

// compile-time assertion: Supervisor must satisfy sim.SubscribeClient.
var _ sim.SubscribeClient = (*Supervisor)(nil)

// Subscribe registers destination with the bus and returns a
// sim.Subscription whose Messages channel receives every frame delivered
// to it, ACROSS reconnects, until ctx is canceled or the bus is declared
// permanently unrecoverable.
//
// The first Subscribe also starts the health watchdog, bound to that
// call's ctx. cmd/bridge subscribes both simulation destinations under
// the same run context, so one watchdog covers both.
func (s *Supervisor) Subscribe(ctx context.Context, destination string) (sim.Subscription, error) {
	raw := make(chan cimstomp.Message) // unbuffered handoff from handler to relay
	ss := &supervisedSub{
		dest: destination,
		sub:  &subscription{msgs: make(chan cimstomp.Message, subscriptionMsgBuf)},
		stop: make(chan struct{}),
	}

	// The handler is registered verbatim again on every reconnect, so it
	// must close over raw and ss only, never over a per-connection
	// token or subscription object.
	ss.handler = func(headers map[string]string, body []byte) {
		msg := cimstomp.Message{
			Destination: destination,
			Headers:     headers,
			Body:        append([]byte(nil), body...),
		}
		select {
		case raw <- msg:
		case <-ctx.Done():
			// Late delivery after shutdown: drop rather than block.
		case <-ss.stop:
			// Late delivery after the bus was declared unrecoverable.
		}
	}

	tok, err := s.bus.Subscribe(ctx, destination, ss.handler)
	if err != nil {
		return nil, fmt.Errorf("gridappsdclient.Supervisor: subscribe %s: %w", destination, err)
	}

	s.mu.Lock()
	ss.tok = tok
	s.subs = append(s.subs, ss)
	startWatch := !s.watchStarted
	s.watchStarted = true
	s.mu.Unlock()

	go s.relay(ctx, ss, raw)
	if startWatch {
		if s.probeDest == "" {
			log.Printf("gridappsdclient.Supervisor: ERROR no probe destination configured; " +
				"a dead broker connection will NOT be detected and this subscriber will go silently deaf")
		} else {
			log.Printf("gridappsdclient.Supervisor: bus health watchdog started probe=%s interval=%s",
				s.probeDest, s.probeInterval)
			go s.watch(ctx)
		}
	}

	return ss.sub, nil
}

// relay is the sole owner and closer of ss.sub.msgs. It forwards raw to
// msgs with a blocking, ctx-guarded send until ctx is done or the bus is
// declared unrecoverable, then unsubscribes (bounded, so an unresponsive
// broker cannot stall teardown past SIGINT) and closes msgs.
//
// This mirrors Subscriber.relay's contract, with one addition: the
// ss.stop arm. Subscriber ends only on ctx cancel because it has no way
// to learn about anything else; Supervisor DOES learn, and a subscriber
// that cannot be restored must end loudly rather than linger on an open
// channel that will never carry another frame.
func (s *Supervisor) relay(ctx context.Context, ss *supervisedSub, raw <-chan cimstomp.Message) {
	defer close(ss.sub.msgs)

	for {
		select {
		case <-ctx.Done():
			s.teardown(ss)
			ss.sub.setErr(ctx.Err())
			return
		case <-ss.stop:
			// fail already recorded the cause on ss.sub; setErr keeps
			// the first error, so the teardown below cannot mask it.
			s.teardown(ss)
			return
		case msg := <-raw:
			select {
			case ss.sub.msgs <- msg:
			case <-ctx.Done():
				s.teardown(ss)
				ss.sub.setErr(ctx.Err())
				return
			case <-ss.stop:
				s.teardown(ss)
				return
			}
		}
	}
}

// teardown unsubscribes ss from the bus, bounded by unsubscribeTimeout.
// It uses the same abandon-the-wait-not-the-goroutine shape as
// Subscriber.relay's shutdown, and for the same reason: the production
// bus delegates to go-stomp's Unsubscribe, which ignores ctx entirely
// and blocks on its own 30s receipt timeout, so racing the call against
// a timer is the only way to bound teardown.
func (s *Supervisor) teardown(ss *supervisedSub) {
	s.mu.Lock()
	tok := ss.tok
	s.mu.Unlock()

	unsubCtx, cancel := context.WithTimeout(context.Background(), unsubscribeTimeout)
	defer cancel()

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- s.bus.Unsubscribe(unsubCtx, ss.dest, tok)
	}()

	select {
	case err := <-resultCh:
		if err != nil {
			ss.sub.setErr(fmt.Errorf("gridappsdclient.Supervisor: unsubscribe %s: %w", ss.dest, err))
		}
	case <-unsubCtx.Done():
		ss.sub.setErr(fmt.Errorf("gridappsdclient.Supervisor: unsubscribe %s: %w", ss.dest, unsubCtx.Err()))
		go func() {
			if err := <-resultCh; err != nil {
				log.Printf("gridappsdclient.Supervisor: abandoned unsubscribe %s completed after the shutdown bound: %v",
					ss.dest, err)
			}
		}()
	}
}

// watch is the health watchdog: one goroutine per Supervisor, started by
// the first Subscribe. Every probeInterval it runs the liveness probe;
// a failed probe means the connection is gone, which is logged at ERROR
// and then recovered. A recovery that cannot complete fails every
// subscription (see recover).
func (s *Supervisor) watch(ctx context.Context) {
	ticker := time.NewTicker(s.probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.runProbe(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("gridappsdclient.Supervisor: ERROR bus liveness probe on %s failed, the broker connection is gone: %v",
					s.probeDest, err)
				if rerr := s.recover(ctx); rerr != nil {
					if ctx.Err() != nil {
						return
					}
					log.Printf("gridappsdclient.Supervisor: ERROR bus recovery abandoned, failing every subscription: %v", rerr)
					s.fail(rerr)
					return
				}
			}
		}
	}
}

// runProbe runs the configured probe bounded by probeTimeout. A probe
// that does not return in time is a failure, not an unknown: the bus not
// answering IS the condition being detected. Fail closed.
func (s *Supervisor) runProbe(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, s.probeTimeout)
	defer cancel()

	resultCh := make(chan error, 1)
	go func() { resultCh <- s.probe(probeCtx) }()

	select {
	case err := <-resultCh:
		return err
	case <-probeCtx.Done():
		return fmt.Errorf("liveness probe did not return within %s: %w", s.probeTimeout, probeCtx.Err())
	}
}

// probeBus is the production liveness probe: subscribe to probeDest and
// then drop it again.
//
// The SUBSCRIBE is the signal. On a connection go-stomp has torn down
// (which is what the 15s read timeout does), Conn.Subscribe returns
// ErrClosedUnexpectedly immediately from its local closed flag
// (conn.go:713-719), so a dead connection is detected without waiting on
// the broker at all.
//
// The UNSUBSCRIBE is hygiene, not signal, and its failure is logged
// rather than reported: go-stomp's Unsubscribe waits for a broker
// RECEIPT on a 30s timeout, and treating a slow receipt as "the
// connection is dead" would tear down a healthy bus. The probe must not
// be able to cause the outage it exists to detect.
func (s *Supervisor) probeBus(ctx context.Context) error {
	noop := func(map[string]string, []byte) {}

	tok, err := s.bus.Subscribe(ctx, s.probeDest, noop)
	if err != nil {
		return fmt.Errorf("probe subscribe %s: %w", s.probeDest, err)
	}
	if uerr := s.bus.Unsubscribe(ctx, s.probeDest, tok); uerr != nil {
		log.Printf("gridappsdclient.Supervisor: probe unsubscribe %s failed (probe itself succeeded): %v",
			s.probeDest, uerr)
	}
	return nil
}

// recover reconnects the bus and resubscribes every destination, with
// capped exponential backoff. Every failed attempt is logged at ERROR:
// a bridge that is running but deaf is worse than one that exits,
// because an operator sees a healthy process and concludes the
// simulation is sending nothing.
//
// Returning an error is the decision that a subscription which cannot be
// restored is FATAL rather than silently degraded. The caller (watch)
// fails every subscription, which closes the consumers' Messages
// channels with a non-nil Err; in cmd/bridge that propagates out of
// runControlSubscriber and runPump and takes the bridge down. That is
// deliberate: the deaf-but-alive state is exactly the failure mode this
// card exists to remove, so recreating a quieter version of it as the
// give-up behavior would defeat the fix.
func (s *Supervisor) recover(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= s.maxRecoverAttempts; attempt++ {
		err := s.reconnect(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		log.Printf("gridappsdclient.Supervisor: ERROR bus reconnect attempt %d/%d failed: %v",
			attempt, s.maxRecoverAttempts, err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt == s.maxRecoverAttempts {
			break
		}
		if !s.sleep(ctx, backoffFor(attempt)) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("%w after %d attempts: %w", ErrBusUnrecovered, s.maxRecoverAttempts, lastErr)
}

// reconnect performs one full recovery attempt: drop the dead
// connection, dial a fresh one (which re-runs the two-step GOSS token
// bootstrap inside gridappsd.Connect: the token-refresh path is reused,
// not reimplemented here), and resubscribe every destination.
//
// Resubscribing is not optional bookkeeping. A reconnect without it
// leaves a live socket with no server-side subscriptions, which looks
// healthier than the dead connection while being just as deaf.
func (s *Supervisor) reconnect(ctx context.Context) error {
	// A Disconnect on an already-dead connection is expected to error;
	// it is logged rather than returned, because failing the attempt
	// here would prevent the Connect that actually fixes things.
	if err := s.bus.Disconnect(); err != nil {
		log.Printf("gridappsdclient.Supervisor: bus disconnect before reconnect returned %v (expected on a dead connection)", err)
	}
	if err := s.bus.Connect(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	s.mu.Lock()
	subs := make([]*supervisedSub, len(s.subs))
	copy(subs, s.subs)
	s.mu.Unlock()

	for _, ss := range subs {
		tok, err := s.bus.Subscribe(ctx, ss.dest, ss.handler)
		if err != nil {
			return fmt.Errorf("resubscribe %s: %w", ss.dest, err)
		}
		s.mu.Lock()
		ss.tok = tok
		s.mu.Unlock()
		log.Printf("gridappsdclient.Supervisor: resubscribed %s on the new connection", ss.dest)
	}

	log.Printf("gridappsdclient.Supervisor: bus reconnected and %d subscription(s) restored", len(subs))
	return nil
}

// fail records cause on every subscription and ends every relay. setErr
// keeps the first error, so recording the cause BEFORE closing stop is
// what makes Subscription.Err report the real reason rather than the
// teardown's own unsubscribe failure.
func (s *Supervisor) fail(cause error) {
	s.mu.Lock()
	subs := make([]*supervisedSub, len(s.subs))
	copy(subs, s.subs)
	s.mu.Unlock()

	for _, ss := range subs {
		ss.sub.setErr(fmt.Errorf("gridappsdclient.Supervisor: subscription %s ended: %w", ss.dest, cause))
		ss.stopOnce.Do(func() { close(ss.stop) })
	}
}

// backoffFor returns the delay before reconnect attempt+1, doubling from
// recoverBackoffBase and capped at recoverBackoffMax.
func backoffFor(attempt int) time.Duration {
	d := recoverBackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= recoverBackoffMax {
			return recoverBackoffMax
		}
	}
	return d
}

// sleepCtx waits d or until ctx is done, reporting true if the full
// duration elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
