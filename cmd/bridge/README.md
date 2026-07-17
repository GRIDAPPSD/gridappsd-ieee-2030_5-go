# cmd/bridge

Stage 1 of the GridAPPS-D side of the IEEE 2030.5 to GridAPPS-D bridge.
The Stage 2 IEEE 2030.5 server embedding is a Stage 2 follow-up filed
separately and requires `ieee-2030_5-go/internal/server` to expose a
public Server constructor.

## What it does

1. Connects to the GridAPPS-D broker over
   [gridappsd-go](https://github.com/GRIDAPPSD/gridappsd-go)'s
   `fieldbus.MessageBus`, adapted to this bridge's own CIM and
   simulation-subscribe interfaces via `internal/gridappsdclient`.
2. Runs the two-step GOSS token authentication as part of that connect.
3. Queries the configured CIM feeder for inverter / solar / battery DERs.
4. Populates an in-memory `internal/registry` Registry with one entry
   per device, keyed by mRID. The LFDI is a deterministic SHA-256
   placeholder over the mRID; real LFDI from device certificates is
   Stage 2 work.
5. If `SEP2_SIMULATION_ID` is set, subscribes to
   `/topic/goss.gridappsd.simulation.output.<sim_id>` and logs each
   `MeasurementFrame`.
6. Idles until SIGINT or SIGTERM. Cancellation flows through one
   `context.Context` root; the message bus and pump goroutines all
   exit on cancel.

## Configuration

All knobs are env-var driven with `gridappsd-docker` defaults. Flags
shadow envs, envs shadow compiled-in defaults.

| Env var | Flag | Default | Notes |
|---|---|---|---|
| `SEP2_STOMP_ADDR` | `-stomp-addr` | `127.0.0.1:61613` | host:port of the broker |
| `SEP2_STOMP_USER` | `-stomp-user` | `system` | gridappsd-docker default |
| `SEP2_STOMP_PASSWORD` | `-stomp-password` | `manager` | gridappsd-docker default |
| `SEP2_SIMULATION_ID` | `-simulation-id` | (empty) | empty disables sim subscribe |
| `SEP2_FEEDER_MRID` | `-feeder-mrid` | `_C1C3E687-6FFD-C753-582B-632A27E28507` | IEEE 123-bus default |
| `SEP2_PUBLISH_ON_START` | `-publish-on-start` | `false` | Stage 2 follow-up; logs and skips |
| `SEP2_STOMP_ALLOW_PLAINTEXT` | `-stomp-allow-plaintext` | `false` | dev-only; gridappsd-docker's dev broker is plain TCP and needs this set to `true` |

The plaintext default is fail-closed: with no override, the bridge
dials TLS against the system trust store. Set
`SEP2_STOMP_ALLOW_PLAINTEXT=true` (or `-stomp-allow-plaintext`) only
against a broker known to be plaintext, such as the local
gridappsd-docker dev stack below.

Run `bridge -h` for the live help.

## Local run

Bring up a broker. Either:

```bash
# Bare ActiveMQ from this repo (good for testing connect / token bootstrap).
cd ~/repos/gridappsd-ieee-2030_5-go
docker compose up -d
```

or:

```bash
# Full GridAPPS-D platform from sentient_gridappsd_integration.
cd ~/repos/sentient_gridappsd_integration/gridappsd-docker
docker compose up -d
# Wait 30 to 60 seconds for the platform to settle.
```

Then run the bridge:

```bash
cd ~/repos/gridappsd-ieee-2030_5-go
make bridge-e2e
```

Or override the env per invocation. Both dev brokers above are plain
TCP, so `SEP2_STOMP_ALLOW_PLAINTEXT=true` is required:

```bash
make bridge-e2e \
  SEP2_STOMP_ADDR=127.0.0.1:61613 \
  SEP2_STOMP_ALLOW_PLAINTEXT=true \
  SEP2_SIMULATION_ID=1234567890
```

The bridge logs to stderr and stays running until Ctrl-C. CIM-query
failures are fatal at startup (no useful work without a feeder); the
subscribe loop logs frame errors and continues.

## Out of scope (Stage 2)

- IEEE 2030.5 server embedding.
- Real LFDI computation from device certificates.
- DERControl translation back to DifferenceBuilder envelopes (currently
  the `-publish-on-start` smoke test is wired but no-ops; building and
  sending the envelope body is a separate follow-up).
- Resubscribe-on-Reconnect for the simulation output topic.
