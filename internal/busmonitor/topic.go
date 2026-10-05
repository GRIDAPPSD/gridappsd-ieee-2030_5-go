// Package busmonitor watches operator-chosen broker topics for the admin UI.
//
// A topic the broker refuses closes the whole STOMP connection it was asked
// on (go-stomp closes the socket on any ERROR frame), so every watched topic
// gets its own connection and never touches the bridge's own bus.
package busmonitor

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Limits fixed by the design. They are exported so the UI layer can show them
// and tests can pin them.
const (
	MaxTopics         = 8
	RingSize          = 200
	MaxBodyBytes      = 8 * 1024
	MaxReconnectTries = 5
	BackoffMin        = 1 * time.Second
	BackoffMax        = 30 * time.Second
	IdleClose         = 30 * time.Second
	maxReasonBytes    = 512
)

var (
	ErrInvalidTopic   = errors.New("busmonitor: topic must be /topic/ plus 1 to 200 characters of dot-separated segments; a segment is A-Z a-z 0-9 _ - or *, and > may only end the name")
	ErrProbeTopic     = errors.New("busmonitor: topic is the bus health probe and cannot be watched")
	ErrTooManyTopics  = fmt.Errorf("busmonitor: at most %d topics can be watched at once", MaxTopics)
	ErrSensitiveTopic = errors.New("busmonitor: topic can carry credentials or connection details and cannot be watched")
	ErrClosed         = errors.New("busmonitor: monitor closed")
	ErrSlowViewer     = errors.New("busmonitor: viewer closed because it did not keep up")
)

const (
	topicPrefix  = "/topic/"
	maxNameBytes = 200
)

// segmentPattern is one literal name segment. Queues are refused by the
// prefix check (they would make the monitor a competing consumer); spaces,
// slashes and control bytes by this class. Go's $ matches only at the end of
// the text, so a trailing newline does not slip through.
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// parseTopic splits name into its dot-separated segments after /topic/.
// A segment is a literal, or exactly "*" (one segment), or ">" as the whole
// final segment (one or more trailing segments). The broker accepts malformed
// patterns such as "goss.>.simulation" silently, so the check is ours.
func parseTopic(name string) ([]string, bool) {
	rest, ok := strings.CutPrefix(name, topicPrefix)
	if !ok || len(rest) < 1 || len(rest) > maxNameBytes {
		return nil, false
	}
	segs := strings.Split(rest, ".")
	for i, seg := range segs {
		switch {
		case seg == "*":
		case seg == ">" && i == len(segs)-1:
		case segmentPattern.MatchString(seg):
		default:
			return nil, false
		}
	}
	return segs, true
}

// matches reports whether the ActiveMQ pattern segs would deliver a message
// published to the literal topic lit. The broker delivers the bare parent to
// an "A.>" subscriber too, so ">" matches zero or more segments.
func matches(segs, lit []string) bool {
	for i, seg := range segs {
		if seg == ">" {
			return len(lit) >= i
		}
		if i >= len(lit) || (seg != "*" && seg != lit[i]) {
			return false
		}
	}
	return len(segs) == len(lit)
}

// ValidateTopic checks name before any connection is opened. probe is the
// supervisor's liveness-probe destination: it is refused by name, and so is
// any wildcard that would match it, because a second subscriber there
// interferes with the health check.
func ValidateTopic(name, probe string) error {
	segs, ok := parseTopic(name)
	if !ok {
		return ErrInvalidTopic
	}
	if probe != "" {
		if psegs, ok := parseTopic(probe); ok && matches(segs, psegs) {
			return ErrProbeTopic
		}
		if name == probe {
			return ErrProbeTopic
		}
	}
	for _, sp := range sensitivePatterns {
		if overlaps(segs, sp) {
			return ErrSensitiveTopic
		}
	}
	return nil
}

// sensitivePatterns are the topics a viewer must never see, written without
// the /topic/ prefix. A requested name is refused when any message could match
// both it and one of these, so wildcards that cover them are refused too.
var sensitivePatterns = [][]string{
	// Every cimstomp Connect publishes base64(user:password) to
	// pnnl.goss.token.topic; the ring would keep the credential of each
	// client that connects, and viewers would read it. The whole token
	// namespace goes with it.
	{"pnnl", "goss", "token", ">"},
	// ActiveMQ advisory topics announce every connection, consumer and
	// destination, including client ids and remote addresses.
	{"ActiveMQ", "Advisory", ">"},
}

// overlaps reports whether some literal topic matches both patterns a and b.
func overlaps(a, b []string) bool {
	for i := 0; ; i++ {
		switch {
		// ">" matches zero or more segments, as matches says, and every
		// earlier segment already overlapped.
		case i < len(a) && a[i] == ">", i < len(b) && b[i] == ">":
			return true
		case i >= len(a) || i >= len(b):
			return i >= len(a) && i >= len(b)
		case a[i] != "*" && b[i] != "*" && a[i] != b[i]:
			return false
		}
	}
}
