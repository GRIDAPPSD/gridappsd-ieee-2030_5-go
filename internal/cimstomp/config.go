package cimstomp

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config holds all application configuration.
type Config struct {
	// STTP connection to the IEEE 2664 publisher.
	STTP STTPConfig `json:"sttp"`

	// STOMP connection to the message bus.
	STOMP STOMPConfig `json:"stomp"`

	// Topic controls how STOMP topic paths are constructed.
	Topic TopicConfig `json:"topic"`

	// SignalMappingFile is the path to the signal mapping JSON file.
	SignalMappingFile string `json:"signal_mapping_file"`
}

// STTPConfig defines the IEEE 2664 / STTP subscriber connection.
type STTPConfig struct {
	// Address is the STTP publisher host:port.
	Address string `json:"address"`

	// FilterExpression selects which measurements to subscribe to.
	// Examples: "FILTER TOP 20 ActiveMeasurements WHERE True",
	//           "FILTER ActiveMeasurements WHERE SignalType='FREQ'"
	FilterExpression string `json:"filter_expression"`
}

// STOMPConfig defines the ActiveMQ / STOMP publisher connection.
type STOMPConfig struct {
	Address  string `json:"address"`
	User     string `json:"user"`
	Password string `json:"password"`

	// TLS, when non-nil, wraps the STOMP TCP connection with crypto/tls.
	// Nil means plain TCP. Caller owns cert-loading policy: for mutual
	// TLS, populate Certificates (client cert plus key), RootCAs (server's
	// CA), MinVersion (recommend TLS 1.2 or 1.3), and ServerName for
	// hostname verification. cimstomp does not introduce helper types;
	// the standard-library *tls.Config is the contract.
	//
	// The TLS field is intentionally not JSON-tagged: TLS material
	// (certs, keys, root pools) does not round-trip cleanly through JSON
	// and should be loaded by the application from disk, env, or a vault
	// before constructing the STOMPConfig.
	//
	// MinVersion is enforced to a floor of tls.VersionTLS12 by the dial
	// helper if the caller leaves it as zero; explicit caller settings
	// (TLS 1.3, etc.) are preserved as-is. InsecureSkipVerify is NOT
	// gated by this package; if a caller sets it to true, server cert
	// verification is disabled and MITM protection is lost. Avoid in
	// production. cimstomp will dial whatever the caller hands it.
	TLS *tls.Config `json:"-"`
}

// TopicConfig controls how measurement topics are built.
// Prefix is the root of every topic. Segments lists which fields
// from the signal mapping appear in the topic path after the prefix.
//
// Available segment names: device, meas_type, phase, component
//
// Example with all segments:
//
//	prefix: "/topic/pmu", segments: ["device","meas_type","phase","component"]
//	result: /topic/pmu.PMU-001.voltage.a.magnitude
//
// Example with fewer segments:
//
//	prefix: "/topic/pmu", segments: ["device","meas_type"]
//	result: /topic/pmu.PMU-001.voltage
type TopicConfig struct {
	Prefix   string   `json:"prefix"`
	Segments []string `json:"segments"`
}

// ValidSegments are the allowed segment names for topic construction.
var ValidSegments = map[string]bool{
	"device":    true,
	"meas_type": true,
	"phase":     true,
	"component": true,
}

// Load reads a Config from the given JSON file path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	return cfg, cfg.validate()
}

func (c *Config) validate() error {
	if c.STTP.Address == "" {
		return fmt.Errorf("sttp.address is required")
	}
	if c.STOMP.Address == "" {
		return fmt.Errorf("stomp.address is required")
	}
	if c.Topic.Prefix == "" {
		return fmt.Errorf("topic.prefix is required")
	}
	if len(c.Topic.Segments) == 0 {
		return fmt.Errorf("topic.segments must contain at least one segment")
	}
	for _, seg := range c.Topic.Segments {
		if !ValidSegments[seg] {
			valid := make([]string, 0, len(ValidSegments))
			for k := range ValidSegments {
				valid = append(valid, k)
			}
			return fmt.Errorf("invalid topic segment %q, valid: %s", seg, strings.Join(valid, ", "))
		}
	}
	return nil
}
