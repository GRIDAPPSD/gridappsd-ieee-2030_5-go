package cimstomp

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/go-stomp/stomp/v3"
	"github.com/go-stomp/stomp/v3/frame"
)

// Sample is a single timestamped measurement value.
type Sample struct {
	Value     float64
	Timestamp int64 // nanoseconds since epoch
	Quality   string
}

// PointMessage is a batch of samples for a single measurement point,
// ready to publish to a STOMP topic.
type PointMessage struct {
	Topic   string
	MRID    string
	Samples []Sample
}

// Publisher manages the STOMP connection and publishes CIM messages.
type Publisher struct {
	cfg  STOMPConfig
	conn *stomp.Conn
}

// New creates a Publisher from STOMP config. When cfg.TLS is non-nil the
// dial path uses crypto/tls; nil keeps the existing plain-TCP behavior.
func New(cfg STOMPConfig) *Publisher {
	return &Publisher{cfg: cfg}
}

// Connect establishes the STOMP connection. The provided context bounds
// both the underlying TCP dial and the STOMP handshake.
//
// go-stomp v3.1.5's DialWithContext calls net.Dial (not net.DialContext),
// so we dial ourselves with net.DialContext to honor ctx, then hand the
// live conn to stomp.ConnectWithContext for the STOMP handshake. When the
// originating STOMPConfig had a non-nil TLS field, the dial wraps the TCP
// connection with crypto/tls.
func (p *Publisher) Connect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	tcp, err := dialSTOMPTransport(ctx, p.cfg)
	if err != nil {
		return err
	}

	// The heartbeat request is asymmetric on purpose, matching
	// Client.dialAndBootstrap: we promise to SEND one every `heartbeat`,
	// and we request NONE inbound (the second argument is 0). A
	// symmetric request had this same dial site dying silently after an
	// idle period, the Publisher-side half of the same defect: see
	// Client.dialAndBootstrap's comment in client.go for the full
	// go-stomp read-deadline mechanics.
	conn, err := stomp.ConnectWithContext(ctx, tcp,
		stomp.ConnOpt.Login(p.cfg.User, p.cfg.Password),
		stomp.ConnOpt.HeartBeat(heartbeat, 0),
		stomp.ConnOpt.Header(frame.ContentType, "application/json"),
	)
	if err != nil {
		_ = tcp.Close()
		return fmt.Errorf("cimstomp.Publisher: stomp connect %s: %w", p.cfg.Address, err)
	}
	p.conn = conn
	log.Printf("STOMP connected to %s", p.cfg.Address)
	return nil
}

// Publish sends a PointMessage to its topic. The JSON payload contains
// an array of samples to handle multiple values per timeslice.
//
// Format:
//
//	{"mRID":"...","values":[{"v":1.02,"ts":1711300000000000,"q":"GOOD"},...]}
//
// Errors:
//   - ErrNotConnected if Connect has not run.
//   - Wrapped ErrConnectionLost if the broker dropped the connection.
//
// Publisher has NO Reconnect primitive (unlike Client), and none is
// planned for v0 (reaffirmed by Leon L2). On
// ErrConnectionLost, the recovery path is: call Close on the old
// Publisher, then construct a fresh Publisher via New and Connect it.
// Publisher's smaller lifecycle (no mutex, no atomics, no in-flight
// request bookkeeping) is the reason a mirror of Client.Reconnect was
// judged not worth the added complexity here; reconstructing is cheap
// because Publisher carries no session-scoped state beyond the single
// *stomp.Conn.
func (p *Publisher) Publish(msg *PointMessage) error {
	if p.conn == nil {
		return ErrNotConnected
	}

	payload := formatPayload(msg)
	if err := p.conn.Send(msg.Topic, "application/json", []byte(payload)); err != nil {
		return wrapTransportErr(fmt.Sprintf("cimstomp.Publisher: send %s", msg.Topic), err)
	}
	return nil
}

// Close disconnects from STOMP. Returns the broker disconnect error, if
// any, after logging it; the caller will already be tearing down the
// connection so the error is reported but not actionable.
func (p *Publisher) Close() error {
	if p.conn == nil {
		return nil
	}
	err := p.conn.Disconnect()
	logDisconnectErr(err, "Publisher.Close")
	p.conn = nil
	log.Println("STOMP disconnected")
	if err != nil {
		return fmt.Errorf("cimstomp.Publisher: disconnect: %w", err)
	}
	return nil
}

// logDisconnectErr logs a Disconnect error during cleanup. We do not fail
// the operation on this; the connection is being torn down anyway. But a
// silent swallow can mask broker-side state leaks (Leon H2).
//
// The logged err can carry a broker-controlled string (an ERROR frame's
// body, surfaced through go-stomp's error chain) verbatim into
// log.Printf (Leon L1, note-only: no operator-facing log
// infrastructure exists yet for this bridge, so there is nothing to
// sanitize against today). If the bridge later gains a structured or
// forwarded logging path (a log aggregator, an operator-facing
// dashboard, anything a broker operator could feed crafted content
// into), sanitize or bound this string before it reaches that sink.
func logDisconnectErr(err error, where string) {
	if err != nil {
		log.Printf("cimstomp: disconnect error during %s: %v", where, err)
	}
}

// formatPayload builds the JSON string without encoding overhead.
func formatPayload(msg *PointMessage) string {
	var b strings.Builder
	b.Grow(64 + 48*len(msg.Samples))

	b.WriteString(`{"mRID":"`)
	b.WriteString(msg.MRID)
	b.WriteString(`","values":[`)

	for i, s := range msg.Samples {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"v":%g,"ts":%d,"q":"%s"}`, s.Value, s.Timestamp, s.Quality)
	}

	b.WriteString("]}")
	return b.String()
}
