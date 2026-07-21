package cimstomp

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/go-stomp/stomp/v3"
)

// Sentinel errors returned by the cimstomp package. Both Client and
// Publisher report not-connected via ErrNotConnected so callers can use
// errors.Is across the two types.
var (
	// ErrNotConnected is returned when Request or Publish is called before
	// Connect or after Close.
	ErrNotConnected = errors.New("cimstomp: not connected")

	// ErrRequestTimeout is returned when the request context deadline
	// expires before the broker delivers a response on the per-request
	// reply-to queue.
	ErrRequestTimeout = errors.New("cimstomp: request timeout")

	// ErrClosed is returned when Connect or Reconnect is called on a Client
	// that has already been closed. Clients are single-shot once Closed:
	// to reuse a lifecycle, construct a new one via NewClient. The guard
	// is enforced in Client.Connect and Client.Reconnect; declaration
	// lives here so both Client and any future Publisher consumers can
	// reference the same sentinel.
	ErrClosed = errors.New("cimstomp: client closed")

	// ErrConnectionLost wraps transport-level failures (broker drop, TCP
	// EOF, half-closed socket) so callers can errors.Is on a single
	// sentinel and decide on Reconnect policy. Returned by Request,
	// Subscribe, and Publisher.Publish whenever the underlying go-stomp
	// or net layer surfaces a known connection-loss error. Application-
	// layer errors (broker ERROR frames, malformed payloads) are NOT
	// wrapped with this sentinel; only the transport drop is.
	//
	// v0 detects connection loss actively: callers observe the error
	// from a Request/Publish call. Passive heartbeat-driven detection
	// is a future enhancement (see GAGO-012 follow-ups).
	ErrConnectionLost = errors.New("cimstomp: connection lost")
)

// wrapTransportErr returns nil for a nil err. For a non-nil err, it
// wraps the cause with the given prefix and chains ErrConnectionLost
// in the error tree when the cause matches a known transport-level
// failure. Callers can errors.Is(err, ErrConnectionLost) to branch on
// Reconnect.
//
// The transport-failure set is intentionally narrow: go-stomp's two
// connection-state errors (ErrAlreadyClosed for use-after-close and
// ErrClosedUnexpectedly for broker drop), plus io.EOF (TCP half-close
// observed via Read) and net.ErrClosed (use-after-close on a *net.Conn).
// Adding a new entry here is a deliberate widening; do not add
// stomp.Error frame errors (those are application-layer broker
// rejections, not transport drops).
//
// ErrAlreadyClosed's inclusion here is deliberate, not loose (GAGO-024
// Dutch M4). It reads like a pure programmer-error sentinel ("you called
// Send after Close"), but go-stomp's *Conn also sets its internal
// closed flag, and therefore returns ErrAlreadyClosed, when the read
// loop observes a broker ERROR frame or a concurrent goroutine calls
// Disconnect: both are broker-drop-shaped events, not caller mistakes.
// A caller cannot tell those two origins apart from the error alone, and
// the correct remedial action (Reconnect) is identical either way, so
// ErrAlreadyClosed is wrapped into ErrConnectionLost alongside
// ErrClosedUnexpectedly rather than singled out as programmer error.
func wrapTransportErr(prefix string, err error) error {
	if err == nil {
		return nil
	}
	if isTransportErr(err) {
		return fmt.Errorf("%s: %w", prefix, errors.Join(ErrConnectionLost, err))
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// isTransportErr reports whether err matches one of the known
// transport-drop conditions. Centralized so wrapTransportErr and any
// future caller use the same set.
func isTransportErr(err error) bool {
	switch {
	case errors.Is(err, stomp.ErrAlreadyClosed),
		errors.Is(err, stomp.ErrClosedUnexpectedly),
		errors.Is(err, io.EOF),
		errors.Is(err, net.ErrClosed):
		return true
	}
	return false
}
