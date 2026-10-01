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

| Env var | Flag | Default | Notes |
|---|---|---|---|
| `SEP2_STOMP_ADDR` | `-stomp-addr` | `127.0.0.1:61613` | Broker `host:port`. |
| `SEP2_STOMP_USER` | `-stomp-user` | `system` | Broker login. |
| `SEP2_STOMP_PASSWORD` | `-stomp-password` | `manager` | Broker password. Credential; see the note above. |
| `SEP2_STOMP_ALLOW_PLAINTEXT` | `-stomp-allow-plaintext` | `false` | Fail-closed: with no override the bridge dials TLS against the system trust store. Set `true` only against a broker known to be plaintext, such as a local dev stack. |
| `SEP2_SIMULATION_ID` | `-simulation-id` | (empty) | GridAPPS-D `simulation_id` to subscribe to. Empty disables the simulation-output subscribe. |
| `SEP2_FEEDER_MRID` | `-feeder-mrid` | `_C1C3E687-6FFD-C753-582B-632A27E28507` | CIM feeder mRID to enumerate DERs from. The default is the IEEE 123-bus feeder shipped with `gridappsd-docker`. |
| `SEP2_BATTERY_LEG_LIST_FILE` | `-sep2-battery-leg-list-file` | unset | Path to a plain-text file of utility battery leg EnergyConsumer mRIDs, one per line, in the model's form (upper case, no leading underscore); blank lines and `#` comments are ignored. The CIM model carries no marker that tells a battery leg apart from any other load, so these must be named; each is checked at boot against the feeder named by `SEP2_FEEDER_MRID` (must be an EnergyConsumer there, and not also a house load) before it is registered. House loads need no such list: they are found by model structure (a `cim:House` link). Unset means no utility battery legs are registered. |
| `SEP2_PUBLISH_ON_START` | `-publish-on-start` | `false` | Publishes a smoke-test `DifferenceBuilder` envelope after registry bootstrap. Requires `SEP2_SIMULATION_ID` to be set. |

## Embedded IEEE 2030.5 server

| Env var | Flag | Default | Notes |
|---|---|---|---|
| `SEP2_SERVER_ADDR` | `-sep2-server-addr` | `127.0.0.1:8443` | The embedded mTLS listener's bind address. Defaults to loopback only; widen it deliberately, see docs/DOCKER.md for the container case. |
| `SEP2_SERVER_CERT_DIR` | `-sep2-server-cert-dir` | `./sep2-certs` | Directory holding the server's CA and leaf certificate material. **The default is relative to the process's current working directory**, not to the repository. Read [CERTIFICATES.md](CERTIFICATES.md) before running the bridge from anywhere you would not want a private key written. |
| `SEP2_DEVICE_CERT_MODE` | `-sep2-device-cert-mode` | `dev-mint` | `dev-mint` or `preprovisioned`. See [CERTIFICATES.md](CERTIFICATES.md). |
| (none) | `-sep2-registration-pin` | unset | Fleet-wide fallback IEEE 2030.5 registration PIN: an integer from 0 to 999999 with a valid section 6.3.5 check digit. Unset means no fleet-wide fallback. Not env-configurable by design, since it behaves as a shared secret. |
| (none) | `-sep2-registration-pin-file` | unset | Path to a JSON file mapping device LFDI to that device's registration PIN. Unset means no per-device PINs are configured. |
| `SEP2_POLL_RATE` | `-sep2-poll-rate` | unset | Fleet-wide `Registration` poll rate in seconds, advertised to every device. Unset advertises nothing; clients apply the spec default of 900 seconds. |
| `SEP2_POST_RATE` | `-sep2-post-rate` | unset | Fleet-wide `MirrorUsagePoint` post rate in seconds. Unset advertises nothing and leaves each client's own value untouched. |
| `SEP2_NOTIFICATION_ALLOW_LOOPBACK` | `-sep2-notification-allow-loopback` | `false` | Fail-closed: with no override a subscription whose `notificationURI` resolves to a loopback address (127.0.0.0/8, `::1`; a hostname such as `localhost` that resolves there counts too) is refused with 400 at creation. Set `true` only for a test harness whose notification receiver listens on loopback. **Do not set this in production**: it admits any loopback destination on any port, reaching every service this bridge's network namespace exposes there, including its own STOMP broker (`SEP2_STOMP_ADDR`), IEEE 2030.5 listener (`SEP2_SERVER_ADDR`) and admin UI (`SEP2_ADMIN_UI_ADDR`) among others; the admin UI's Bearer auth does not narrow this. Logs a warning once at start-up when set. |

## Admin UI

The admin listener serves the IEEE 2030.5 server's admin UI (at `/ui/`) and
admin API, with six bridge tabs after the server's own: bridge health,
registry, discovered DERs, served resources, connected clients and control
flow. It is off by default: an unset or blank `SEP2_ADMIN_UI_KEY` disables
it entirely (logged at start-up) rather than serving anything
unauthenticated. A key that is set but shorter than 16 characters stops the
bridge at start-up with an error naming the rule.

Every route needs the key, from loopback too, except the login page and
form. The plane is read-only: no admin write route is mounted (the bridge
seeds and writes the stores itself), so a write button in the server's UI
gets 404 or 405. `/api/health` and `/api/clients` stay as Bearer-only JSON
routes for scripts.

| Env var | Flag | Default | Notes |
|---|---|---|---|
| `SEP2_ADMIN_UI_ADDR` | `-admin-ui-addr` | `127.0.0.1:8444` | Admin UI listener bind address. Loopback only unless `SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK` is also set. |
| `SEP2_ADMIN_UI_KEY` | `-admin-ui-key` | (empty, disabled) | Admin credential: the Bearer token and the login password, at least 16 characters. Credential; see the note above. |
| `SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK` | `-admin-ui-allow-non-loopback` | `false` | Explicit opt-in to bind the admin UI to a non-loopback host. |
| `SEP2_ADMIN_UI_ALLOWED_HOSTS` | (none) | (empty) | Comma-separated extra accepted `Host` header values, in addition to the built-in `localhost`, `127.0.0.1`, and `::1`. |
| `SEP2_ADMIN_UI_SOR_LINK` | `-admin-ui-sor-link` | (empty) | Optional server-of-record dashboard URL, returned read-only from `/api/health`. Not a credential. |

## Other

`-version` prints the build version and exits. It takes no
environment variable and has no side effect: it returns before any
network I/O or credential handling runs.

## Source

Read end to end from `cmd/bridge/config.go`, current as of the commit
this document ships with. Consult that file directly if a flag or
default appears to have changed.
