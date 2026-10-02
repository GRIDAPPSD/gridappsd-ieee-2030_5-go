# cmd/bridge

The bridge binary's entry point.

## What it does

1. Connects to the GridAPPS-D broker over
   [gridappsd-go](https://github.com/GRIDAPPSD/gridappsd-go)'s
   `fieldbus.MessageBus`, including the two-step GOSS token
   authentication.
2. Queries the configured CIM feeder for inverter, solar, and battery
   PowerElectronicsConnection DERs, plus (see
   [../../docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)) any
   EnergyConsumer house loads the model marks structurally and any
   utility battery legs named in `SEP2_BATTERY_LEG_LIST_FILE`.
3. Derives a real, certificate-backed IEEE 2030.5 identity (LFDI per
   spec section 6.3.4, SFDI per section 6.3.3) for each device and
   starts the embedded mTLS server (`internal/sep2embed`) those
   devices connect to, with per-device access control
   (`internal/sep2acl`).
4. If `SEP2_SIMULATION_ID` is set, subscribes to the simulation
   output topic and logs each measurement frame; DERControl flowing
   from devices back onto the GridAPPS-D bus is wired through the
   same path.
5. Idles until SIGINT or SIGTERM. A single `context.Context` root
   cancels the message bus and every server goroutine together.

Configuration is documented in full in
[../../docs/CONFIGURATION.md](../../docs/CONFIGURATION.md).
Certificate setup is in
[../../docs/CERTIFICATES.md](../../docs/CERTIFICATES.md).

## Local run

Bring up a broker. Either the bare ActiveMQ broker in this repo, good
for testing connect and token bootstrap:

```bash
docker compose up -d
```

or the full GridAPPS-D platform, started with the `run.sh` in
wherever you have the `gridappsd-docker` stack cloned (allow 30 to 60
seconds for it to settle). Do not use a bare `docker compose up -d`
there: `run.sh` first sets up what the compose file expects, namely
the image tag and the mysql dump it mounts.

```bash
./run.sh
```

Then, from this repo's root:

```bash
make bridge-e2e
```

Both dev brokers above are plain TCP, so `bridge-e2e`'s default
already sets `SEP2_STOMP_ALLOW_PLAINTEXT=true`. Override any variable
on the make command line:

```bash
make bridge-e2e \
  SEP2_STOMP_ADDR=127.0.0.1:61613 \
  SEP2_SIMULATION_ID=1234567890
```

Do not put `SEP2_STOMP_PASSWORD=...` directly on that command line
with a real password: it lands in your shell history and is visible
to any other local user via `ps` while the command runs. Export it
first instead; see [../../docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)
for the full credential-handling note.

The bridge logs to stderr and runs until Ctrl-C. A CIM query failure
is fatal at startup, since there is no useful work without a feeder;
the simulation subscribe loop logs frame errors and continues.
