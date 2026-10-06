# Configuration

All bridge configuration is read in `cmd/bridge/config.go`. Precedence
is fixed: a command-line flag shadows its environment variable, which
shadows the compiled-in default. Run `bridge -h` for the live help
text.

Credential-bearing flags (`-stomp-user`, `-stomp-password`,
`-admin-ui-key`) register an empty string as their flag default, so
`-h` or a parse error never echoes a real value. Prefer the
environment variable over the flag for any credential: a flag value
is visible in `/proc/<pid>/cmdline` to any local user who can read
that process's `/proc` entry for as long as the process runs, and a
value passed as `VAR=value` on a `make` or shell command line lands
in that shell's history. Export the variable first instead:

```bash
export SEP2_STOMP_PASSWORD=manager
make bridge-e2e SEP2_STOMP_ADDR=127.0.0.1:61613 SEP2_STOMP_ALLOW_PLAINTEXT=true
```

## GridAPPS-D connection

The `GRIDAPPSD_` names let the bridge share an environment with other
GridAPPS-D apps. Precedence for each setting, highest first: flag,
`SEP2_` variable, `GRIDAPPSD_` variable, default. `GRIDAPPSD_ADDRESS`
and `GRIDAPPSD_PORT` each replace their half of `127.0.0.1:61613`; set
one and the other keeps its default. A set `SEP2_STOMP_ADDR` wins whole,
so a `GRIDAPPSD_PORT` beside it is ignored. In the compose file the
login pair is passed through under both names, with empty defaults, so
`GRIDAPPSD_USER` and `GRIDAPPSD_PASSWORD` in `.env` work on their own.
The address pair is not passed; set `SEP2_STOMP_ADDR` to change the
container's broker address.

| Env var | Flag | Default | Notes |
|---|---|---|---|
| `SEP2_STOMP_ADDR` | `-stomp-addr` | `127.0.0.1:61613` | Broker `host:port`. |
| `SEP2_STOMP_USER` | `-stomp-user` | `system` | Broker login. |
| `SEP2_STOMP_PASSWORD` | `-stomp-password` | `manager` | Broker password. Credential; see the note above. |
| `GRIDAPPSD_USER` | (none) | (unset) | Alternate for `SEP2_STOMP_USER`, the name other GridAPPS-D apps read. |
| `GRIDAPPSD_PASSWORD` | (none) | (unset) | Alternate for `SEP2_STOMP_PASSWORD`. Credential: removed from the process environment after reading and never logged. |
| `GRIDAPPSD_ADDRESS` | (none) | (unset) | Broker host; replaces the host of the default address. |
| `GRIDAPPSD_PORT` | (none) | (unset) | Broker port, 1 to 65535; replaces the port of the default address. A bad value stops start-up. |
| `SEP2_STOMP_ALLOW_PLAINTEXT` | `-stomp-allow-plaintext` | `false` | Fail-closed: with no override the bridge dials TLS against the system trust store. Set `true` only against a broker known to be plaintext, such as a local dev stack. |
| `SEP2_SIMULATION_ID` | `-simulation-id` | (empty) | GridAPPS-D `simulation_id` to subscribe to. Controls the simulation-output measurement subscribe, the `simulation_id` field in published status, the liveness-probe topic (the per-simulation log topic when set, `/topic/goss.gridappsd.heartbeat` otherwise). `-publish-on-start` is refused at start-up when this is empty. Empty disables the measurement subscribe only; status publishing and control subscription do not depend on it. |
| `SEP2_APPLICATION_ID` | `-application-id` | `IEEE_2030_5` | GridAPPS-D application id. Device status is published to `/topic/goss.gridappsd.<application id>.output` and controls are read from `/topic/goss.gridappsd.<application id>.input`. Neither topic carries a simulation id, so both work with `SEP2_SIMULATION_ID` unset. The Python service took its application id from `GRIDAPPSD_SERVICE_NAME`; the bridge uses `SEP2_APPLICATION_ID`. Must not be empty. |
| `SEP2_FEEDER_MRID` | `-feeder-mrid` | `_C1C3E687-6FFD-C753-582B-632A27E28507` | CIM feeder mRID to enumerate DERs from. The default is the IEEE 123-bus feeder shipped with `gridappsd-docker`. |
| `SEP2_BATTERY_LEG_LIST_FILE` | `-sep2-battery-leg-list-file` | unset | Path to a plain-text file of utility battery leg EnergyConsumer mRIDs, one per line, in the model's form (upper case, no leading underscore); blank lines and `#` comments are ignored. The CIM model carries no marker that tells a battery leg apart from any other load, so these must be named; each is checked at boot against the feeder named by `SEP2_FEEDER_MRID` (must be an EnergyConsumer there, and not also a house load) before it is registered. House loads need no such list: they are found by model structure (a `cim:House` link). Unset means no utility battery legs are registered. |
| `SEP2_REGISTRATION_PIN` | `-sep2-registration-pin` | unset | Fleet-wide fallback IEEE 2030.5 registration PIN, 0-999999 with a valid section 6.3.5 check digit. Credential: the value is never logged or echoed in an error, and the variable is removed from the bridge process environment after it is read, so child processes do not see it; it stays in `/proc/<pid>/environ` for the life of the process. The flag wins when both are set. Unset means no fleet-wide fallback. |
| `SEP2_REGISTRATION_PIN_FILE` | `-sep2-registration-pin-file` | unset | Path to a flat JSON object mapping device LFDI to that device's PIN, same validation as the PIN above. In the container the path must be inside a mount, such as the certificate directory. The flag wins when both are set. Unset means no per-device PINs. |
| `SEP2_PUBLISH_ON_START` | `-publish-on-start` | `false` | Accepted but does nothing yet: the bridge logs that the `DifferenceBuilder` envelope publish is a follow-up and skips it, and sends nothing. Start-up still refuses it when `SEP2_SIMULATION_ID` is empty. |
| `SEP2_STOMP_CONNECT_TIMEOUT` | `-stomp-connect-timeout` | `15s` | A Go duration such as `15s` or `1m`. Bounds the broker dial plus token bootstrap at start-up. Allowed range: 1s-5m. |
| `SEP2_STOMP_HEARTBEAT` | `-stomp-heartbeat` | `10s` | A Go duration such as `15s` or `1m`. STOMP heartbeat interval offered to the broker on both connection legs. A half-open broker is only torn down after the heartbeat read timeout, so a larger value delays the liveness probe noticing a dead connection by about that much. Allowed range: 1s-5m. |
| `SEP2_STOMP_PROBE_INTERVAL` | `-stomp-probe-interval` | `5s` | A Go duration such as `15s` or `1m`. Gap between broker liveness probes; a dead connection is noticed within about one interval plus the probe timeout. Allowed range: 1s-1h. |
| `SEP2_STOMP_PROBE_TIMEOUT` | `-stomp-probe-timeout` | `10s` | A Go duration such as `15s` or `1m`. A probe that has not returned in this time counts as a failure and triggers a reconnect. Keep it above one broker round trip: a shorter value fails every probe on a healthy bus and the bridge reconnects each interval. Allowed range: 1s-5m. |
| `SEP2_STOMP_RECONNECT_BACKOFF_BASE` | `-stomp-reconnect-backoff-base` | `500ms` | A Go duration such as `15s` or `1m`. First delay between reconnect attempts; it doubles each attempt up to the max. Allowed range: 100ms-1m. |
| `SEP2_STOMP_RECONNECT_BACKOFF_MAX` | `-stomp-reconnect-backoff-max` | `10s` | A Go duration such as `15s` or `1m`. Longest reconnect delay. Start-up refuses a max below the base. Allowed range: 1s-10m. |
| `SEP2_STOMP_UNSUBSCRIBE_TIMEOUT` | `-stomp-unsubscribe-timeout` | `5s` | A Go duration such as `15s` or `1m`. How long shutdown waits for the broker to acknowledge each unsubscribe. Applies to the supervised bus the bridge runs; the unsupervised subscriber keeps a fixed 5s. Allowed range: 1s-5m. |
| `SEP2_CIM_QUERY_TIMEOUT` | `-cim-query-timeout` | `30s` | A Go duration such as `15s` or `1m`. One budget shared by the start-up CIM feeder queries. Allowed range: 1s-10m. |
| `SEP2_HISTORY_LOG_INTERVAL` | `-history-log-interval` | `30s` | A Go duration such as `15s` or `1m`. Minimum gap between two history log lines of the same kind. Allowed range: 1s-1h. |

## Embedded IEEE 2030.5 server

| Env var | Flag | Default | Notes |
|---|---|---|---|
| `SEP2_SERVER_ADDR` | `-sep2-server-addr` | `127.0.0.1:8443` | The embedded mTLS listener's bind address. Defaults to loopback only; widen it deliberately, see docs/DOCKER.md for the container case. |
| `SEP2_SERVER_CERT_DIR` | `-sep2-server-cert-dir` | `./sep2-certs` | Directory holding the server's CA and leaf certificate material. **The default is relative to the process's current working directory**, not to the repository. Read [CERTIFICATES.md](CERTIFICATES.md) before running the bridge from anywhere you would not want a private key written. |
| `SEP2_DEVICE_CERT_MODE` | `-sep2-device-cert-mode` | `dev-mint` | `dev-mint` or `preprovisioned`. See [CERTIFICATES.md](CERTIFICATES.md). |
| `SEP2_SERVER_CERT_HOSTS` | `-sep2-server-cert-hosts` | (empty) | Comma-separated extra DNS names or IP addresses for the server certificate dev-mint creates, added to `localhost` and `127.0.0.1`. Each entry must be an IP or a plain host name (no wildcard, port or scheme). Applies only when the certificate directory is empty: an existing `server.pem` is never re-minted, and the bridge logs a warning when it lacks a requested name. |
| (none) | `-sep2-registration-pin` | unset | Fleet-wide fallback IEEE 2030.5 registration PIN: an integer from 0 to 999999 with a valid section 6.3.5 check digit. Unset means no fleet-wide fallback. Not env-configurable by design, since it behaves as a shared secret. |
| (none) | `-sep2-registration-pin-file` | unset | Path to a JSON file mapping device LFDI to that device's registration PIN. Unset means no per-device PINs are configured. |
| `SEP2_POLL_RATE` | `-sep2-poll-rate` | unset | Fleet-wide `Registration` poll rate in seconds, advertised to every device. Unset advertises nothing; clients apply the spec default of 900 seconds. |
| `SEP2_POST_RATE` | `-sep2-post-rate` | unset | Fleet-wide `MirrorUsagePoint` post rate in seconds. Unset advertises nothing and leaves each client's own value untouched. |
| `SEP2_NOTIFICATION_ALLOW_LOOPBACK` | `-sep2-notification-allow-loopback` | `false` | Fail-closed: with no override a subscription whose `notificationURI` resolves to a loopback address (127.0.0.0/8, `::1`; a hostname such as `localhost` that resolves there counts too) is refused with 400 at creation. Set `true` only for a test harness whose notification receiver listens on loopback. **Do not set this in production**: it admits any loopback destination on any port, reaching every service this bridge's network namespace exposes there, including its own STOMP broker (`SEP2_STOMP_ADDR`), IEEE 2030.5 listener (`SEP2_SERVER_ADDR`) and admin UI (`SEP2_ADMIN_UI_ADDR`) among others; the admin UI's Bearer auth does not narrow this. Logs a warning once at start-up when set. |
| `SEP2_ENABLE_CCM` | `-sep2-enable-ccm` | `false` | Kept as configuration surface pending removal. Every embedded listener already offers only `TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8`, the suite IEEE 2030.5-2018 section 6.7 makes mandatory, so setting this changes no cipher suite; it only turns the connection observer off. Start-up refuses it unless `SEP2_CCM_ALLOW_NO_OBSERVER` is also set. |
| `SEP2_CCM_ALLOW_NO_OBSERVER` | `-sep2-ccm-allow-no-observer` | `false` | The explicit second setting `SEP2_ENABLE_CCM` requires. Accepts losing the connection observer: no rejected-device record, and the admin UI's served-status table shows every device as unknown, with empty connected-clients and handshake-attempts tables. Setting it alone has no effect. Both settings are scheduled for removal. |
| `SEP2_SERVER_READ_HEADER_TIMEOUT` | `-sep2-server-read-header-timeout` | `10s` | A Go duration such as `15s` or `1m`. Protocol listener: time to read a request's headers. Allowed range: 1s-1h. |
| `SEP2_SERVER_READ_TIMEOUT` | `-sep2-server-read-timeout` | `30s` | A Go duration such as `15s` or `1m`. Protocol listener: time to read a whole request. Allowed range: 1s-1h. |
| `SEP2_SERVER_WRITE_TIMEOUT` | `-sep2-server-write-timeout` | `30s` | A Go duration such as `15s` or `1m`. Protocol listener: time to write a response. Allowed range: 1s-1h. |
| `SEP2_SERVER_IDLE_TIMEOUT` | `-sep2-server-idle-timeout` | `120s` | A Go duration such as `15s` or `1m`. Protocol listener: keep-alive idle time. Allowed range: 1s-1h. |
| `SEP2_SERVER_SHUTDOWN_TIMEOUT` | `-sep2-server-shutdown-timeout` | `5s` | A Go duration such as `15s` or `1m`. Bound on the protocol listener's graceful drain at shutdown. Allowed range: 1s-5m. |
| `SEP2_CONTROL_SWEEP_INTERVAL` | `-sep2-control-sweep-interval` | `10s` | A Go duration such as `15s` or `1m`. How often ended DERControls are expired fleet-wide. A longer value lets a client that connects late be served an ended event for longer; it stays a sampling rate, not an event length. Allowed range: 1s-1h. |
| `SEP2_NOTIFY_POST_TIMEOUT` | `-sep2-notify-post-timeout` | `30s` | A Go duration such as `15s` or `1m`. Time one subscription notification POST may take. Allowed range: 1s-5m. |
| `SEP2_NOTIFY_DIAL_TIMEOUT` | `-sep2-notify-dial-timeout` | `30s` | A Go duration such as `15s` or `1m`. Connect budget for one notification POST. The server caps it at `SEP2_NOTIFY_POST_TIMEOUT`, so a value above that has no effect. Allowed range: 1s-5m. |
| `SEP2_NOTIFY_RESOLVE_TIMEOUT` | `-sep2-notify-resolve-timeout` | `5s` | A Go duration such as `15s` or `1m`. Time allowed for the DNS check on a subscription's `notificationURI` when it is created. Allowed range: 1s-5m. |
| `SEP2_CCM_HANDSHAKE_TIMEOUT` | `-sep2-ccm-handshake-timeout` | `10s` | A Go duration such as `15s` or `1m`. Time one inbound TLS handshake may take on the protocol listener before the connection is dropped. Allowed range: 1s-5m. |
| `SEP2_NOTIFY_WORKERS` | `-sep2-notify-workers` | `4` | An integer. Subscription notification worker count. Allowed range: 1-1024. |
| `SEP2_NOTIFY_QUEUE_SIZE` | `-sep2-notify-queue-size` | `100` | An integer. Subscription notification queue length. Allowed range: 1-100000. |

## Admin UI

The admin listener serves the IEEE 2030.5 server's admin UI (at `/ui/`) and
admin API, with bridge tabs after the server's own: bridge health,
connections, DER programs, control flow and the device-status graph. The
registry name, identity and discovered DERs are columns on the server's
Devices tab. It is off by default: an unset or blank `SEP2_ADMIN_UI_KEY` disables
it entirely (logged at start-up) rather than serving anything
unauthenticated. A key that is set but shorter than 16 characters stops the
bridge at start-up with an error naming the rule.

`SEP2_ADMIN_UI_INSECURE_NO_KEY=true` is the explicit opt-in to run the UI
with no key for the operator: leave `SEP2_ADMIN_UI_KEY` blank, and the bridge
holds a random key of its own and authenticates every request with it, so the
UI opens with no login. With it on, anyone who can reach the admin address can read every panel and, when publishing is switched on, send control messages; the publishing switch is reachable too, so the switch protects against accident, not against a person. The Host allowlist and the
cross-origin refusal stay on, so a web page the operator visits cannot drive
it. Setting this together with a key stops the bridge at start-up. The bridge
logs a warning when the listener binds, before it serves, and drops the per-request
`admin_auth_success` log line.

Every route needs the key (unless no-key mode is on), from loopback too, except the login page and
form. The plane is read-only: no admin write route is mounted (the bridge
seeds and writes the stores itself), so a write button in the server's UI
gets 404 or 405. `/api/health`, `/api/clients` and `/api/registry` stay as Bearer-only JSON
routes for scripts.

| Env var | Flag | Default | Notes |
|---|---|---|---|
| `SEP2_ADMIN_UI_ADDR` | `-admin-ui-addr` | `127.0.0.1:8444` | Admin UI listener bind address. Loopback only unless `SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK` is also set. |
| `SEP2_ADMIN_UI_KEY` | `-admin-ui-key` | (empty, disabled) | Admin credential: the Bearer token and the login password, at least 16 characters. Credential; see the note above. |
| `SEP2_ADMIN_UI_INSECURE_NO_KEY` | `-admin-ui-insecure-no-key` | `false` | INSECURE: run the admin UI with no key. A boolean. A blank `SEP2_ADMIN_UI_KEY` still disables the UI when this is false; both set stops the start. See above. |
| `SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK` | `-admin-ui-allow-non-loopback` | `false` | Explicit opt-in to bind the admin UI to a non-loopback host. |
| `SEP2_ADMIN_UI_ALLOWED_HOSTS` | (none) | (empty) | Comma-separated extra accepted `Host` header values, in addition to the built-in `localhost`, `127.0.0.1`, and `::1`. |
| `SEP2_ADMIN_UI_SOR_LINK` | `-admin-ui-sor-link` | (empty) | Optional server-of-record dashboard URL, returned read-only from `/api/health`. Not a credential. |
| `SEP2_ADMIN_UI_BUS_PUBLISH_AT_START` | `-admin-ui-bus-publish-at-start` | `false` | Whether the admin UI sender's publish switch is on at start-up. Off by default, so a restart never re-arms writes to the simulation; the switch is in memory only and returns to this value at every start. While it is off every send is refused. |
| `SEP2_ADMIN_UI_READ_HEADER_TIMEOUT` | `-admin-ui-read-header-timeout` | `5s` | A Go duration such as `15s` or `1m`. Admin listener: time to read a request's headers. Allowed range: 1s-1h. |
| `SEP2_ADMIN_UI_READ_TIMEOUT` | `-admin-ui-read-timeout` | `10s` | A Go duration such as `15s` or `1m`. Admin listener: time to read a whole request. Allowed range: 1s-1h. |
| `SEP2_ADMIN_UI_WRITE_TIMEOUT` | `-admin-ui-write-timeout` | `10s` | A Go duration such as `15s` or `1m`. Admin listener: time to write a response. Allowed range: 1s-1h. |
| `SEP2_ADMIN_UI_IDLE_TIMEOUT` | `-admin-ui-idle-timeout` | `60s` | A Go duration such as `15s` or `1m`. Admin listener: keep-alive idle time. Allowed range: 1s-1h. |
| `SEP2_ADMIN_UI_SHUTDOWN_TIMEOUT` | `-admin-ui-shutdown-timeout` | `5s` | A Go duration such as `15s` or `1m`. Bound on the admin listener's graceful drain at shutdown. Allowed range: 1s-5m. |
| `SEP2_ADMIN_UI_CLIENT_IDLE_AFTER` | `-admin-ui-client-idle-after` | `5m` | A Go duration such as `10m` or `1h`. A client not seen for longer than this shows as idle, not connected, in the Connections panel and in `/api/clients` (`connected` is false); idle clients stay listed. It is also the silence after which the Devices tab shows a device's Comms as offline. The two differ on refused requests: the Devices tab counts only requests the server accepted, while `/api/clients` and the Connections panel also count requests the ACL refused, so a device stuck in a refusal loop reads connected there and offline on the Devices tab. Allowed range: 30s-24h. |

## Server admin-plane settings

The bridge reads four settings the IEEE 2030.5 server defines, with the
server's own parsers, at startup and whether or not the admin UI is enabled.
A value out of range stops the bridge before it listens, and the error names
the variable. They have no flags. An unset or empty variable takes the
server default.

| Env var | Default | Notes |
|---|---|---|
| `SEP2_EDITION` | `2018` | Only `2018` (or unset) is supported. `2023` stops the bridge at startup: its control path reports 2018 statuses and does not wire the stores the server needs for 2023. |
| `SEP2_PEN` | (unset) | IANA Private Enterprise Number, placed in the low 32 bits of the mRIDs the protocol router mints for flow reservation responses. Unset leaves them random. The read-only admin plane does not use it. |
| `SEP2_FLOW_RESERVATION_DEADLINE_SECONDS` | `300` | Whole seconds from 1 to 3600. Reaches the protocol router. The admin plane validates it but no mounted read-only route reads it. |
| `SEP2_FLOW_RESERVATION_RETENTION_GRACE_SECONDS` | `1800` | Whole seconds from 900 to 604800. Validated only: it has no effect in the bridge today, which runs no retention sweep and mounts no route that reads it. |

## Operator policy

These shape what the embedded server advertises and issues. A file path
setting is read once at start-up; a malformed or out-of-range value stops
the bridge before it listens. IEEE 2030.5 range checks happen after
parsing, so the error is the same whichever source supplied the value.
Every flag here registers an empty default, so `-h` shows nothing set.

| Env var | Flag | Default | Notes |
|---|---|---|---|
| (none) | `-sep2-program-file` | unset | Path to a JSON object overriding the seeded default DERProgram: `{"primacy": 1, "description": "..."}`. Both members are optional, an absent one keeps the compiled-in value, and an unknown member is an error. Rewritten by an admin UI, applied on restart. |
| (none) | `-sep2-program-primacy` | unset (`1`) | Primacy of the default DERProgram, 0-2 or 65-191, lower is higher priority. Overrides the file. Unset uses `1`, contracted premises service provider, which suits a co-simulation but not a field deployment, where `1` outranks a DSO program in the 65-191 band; set it per the interconnection agreement. |
| (none) | `-sep2-program-description` | unset (`GridAPPS-D DER program`) | Description of the default DERProgram, at most 32 characters. Overrides the file. |
| (none) | `-sep2-control-duration` | unset (`1800`) | Interval duration in seconds of every issued DERControl, after which the device falls back to DefaultDERControl. 1800 is twice the default poll rate; set it above the configured poll rate or controls expire between polls. |
| (none) | `-sep2-control-randomize-duration` | unset (`0`) | `randomizeDuration` in seconds served on every issued DERControl, -3600 to 3600, staggering when devices revert to DefaultDERControl. 0 keeps co-simulation runs reproducible; a field deployment should set a non-zero value. |
| (none) | `-sep2-default-control-file` | unset | Path to a JSON object overriding the seeded DefaultDERControl: `opModConnect` (boolean), `opModEnergize` (boolean) and `opModMaxLimW` (integer 0 to 10000, hundredths of a percent). Each member is optional and an absent one keeps the compiled-in value; any other member is an error. Unset ships a control that commands nothing, leaving each DER on its own IEEE 1547 autonomous behavior. |

## Telemetry

The DERStatus telemetry publisher always runs and publishes to the application output topic, so it needs no simulation id. When `SEP2_SIMULATION_ID` is set it is carried as `simulation_id` in each message; when empty the field is omitted.

| Env var | Flag | Default | Notes |
|---|---|---|---|
| `SEP2_TELEMETRY_INTERVAL` | `-sep2-telemetry-interval` | `15s` | Publish period as a Go duration such as `15s` or `1m`, not a bare number. Must be greater than zero. |
| `SEP2_TELEMETRY_PUBLISH_UNCHANGED` | `-sep2-telemetry-publish-unchanged` | `false` | By default only devices whose mapped values moved since their last successful publish are sent. `true` publishes every device with a stored DERStatus every interval (full-snapshot semantics); set it if the subscriber treats each message as a complete snapshot. |

## Other

`-version` prints the build version and exits. It takes no
environment variable and has no side effect: it returns before any
network I/O or credential handling runs.

## Source

Read end to end from `cmd/bridge/config.go`, current as of the commit
this document ships with. Consult that file directly if a flag or
default appears to have changed.
