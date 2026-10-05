# Certificates

The bridge's embedded IEEE 2030.5 server needs mTLS identity material
before it can accept any device connection. This document covers what
it needs, where it lives, and how to produce it. It is read from
`internal/sep2embed/certs.go` and `internal/sep2embed/devicecert.go`;
consult those files directly if behavior here appears to have
changed.

**Never place any certificate or key material inside a repository
checkout or a workspace path.** Every example below writes to an
absolute path outside any git working tree. Treat every `.pem` and
`.x509` file the bridge produces as secret, regardless of which of
the six it is.

## Two CAs: the device CA and the serving CA

`SEP2_SERVER_CERT_DIR` (default `./sep2-certs`, resolved relative to
the process's working directory) holds six fixed file names, one pair
per CA plus the server's own leaf:

| File | Purpose |
|---|---|
| `ca.pem` | The **device CA** certificate. Signs every device certificate; the protocol listener's trust bundle for verifying devices. A device never needs this file. |
| `ca-key.pem` | The device CA's private key. Needed only to sign new device certificates. |
| `serving-ca.pem` | The **serving CA** certificate. Signs the bridge's own leaf (`server.pem`) and nothing else. **This is the anchor a device or client is given to verify a freshly minted bridge**, not `ca.pem`; an existing directory that predates the split keeps verifying against `ca.pem` alone, see below. |
| `serving-ca-key.pem` | The serving CA's private key. Needed only to sign the server leaf. |
| `server.pem` | The embedded server's own leaf certificate, signed by the serving CA. |
| `server-key.pem` | The embedded server's private key. |

Splitting the two CAs means a device-CA replacement never strands the
server's own identity, and a serving-CA compromise never lets an
attacker mint a trusted device certificate. Losing either key is bad in
its own way; neither is more expendable than the other.

**The pair is optional as a whole, but only for an existing, already
complete directory.** A directory holding just the original four names
(`ca.pem`, `ca-key.pem`, `server.pem`, `server-key.pem`) with neither
`serving-ca.pem` nor `serving-ca-key.pem` present loads exactly as it
did before the split: the server leaf there was signed before the split
existed, so the device CA still verifies it. **This does not apply to a
fresh mint.** An empty directory in `dev-mint` mode always mints two
distinct CAs; there is no pre-split leaf to stay compatible with, so a
client that holds only `ca.pem` cannot verify a freshly minted bridge.
The bridge logs a loud warning naming both files when this happens; read
it.

Which of the six files are *required* depends on `SEP2_DEVICE_CERT_MODE`,
because only one of the two modes signs anything:

| Mode | Signs? | Required files |
|---|---|---|
| `dev-mint` | yes | `ca.pem`, `ca-key.pem`, `server.pem`, `server-key.pem`, and, once either serving CA file is present, both `serving-ca.pem` and `serving-ca-key.pem` |
| `preprovisioned` | no | `ca.pem`, `server.pem`, `server-key.pem`; once either serving CA file is present, `serving-ca.pem` becomes required too, but `serving-ca-key.pem` never is |

**`preprovisioned` does not require `ca-key.pem` or `serving-ca-key.pem`,
and never reads either.** Keep both signing keys off the bridge host
entirely; issue certificates wherever you keep them, and copy only the
public certificates across. A stray `ca-key.pem` or `serving-ca-key.pem`
an operator forgot to remove is reported as found, but never as
required: the refusal message for this mode never asks for a signing
key it will not read.

The directory is created at mode `0700` and each file at mode `0600`
when the bridge writes them. Match those permissions if you supply
your own.

Per-device certificates, used when `SEP2_DEVICE_CERT_MODE=preprovisioned`,
live under `<SEP2_SERVER_CERT_DIR>/devices/`, one raw DER-encoded
`<name>.x509` file per device, signed by the same CA as the server
identity above. In `dev-mint` mode each freshly minted device also gets
a sibling `<name>.pem` private key, on the same base name, for dev
tooling that dials in as that device; the bridge never reads it back.

A minted server certificate names only `localhost` and `127.0.0.1`. A
client that connects to any other host name or address fails hostname
verification against it, so a non-loopback deployment must supply its
own server certificate (see `preprovisioned` above).

## What the bridge does at startup

It inspects the directory on every start and does exactly one of:

- **Complete for the mode**: loads the files and writes nothing. The
  directory may be read-only.
- **Empty, in `dev-mint`, and writable**: mints a self-signed
  development device CA, a self-signed development serving CA, and a
  server leaf signed by the serving CA; writes all six files; and logs
  a loud warning naming both CAs by file and saying which one a client
  must be given.
- **Anything else**: refuses to start.

**No existing file is ever overwritten.** A directory holding some but
not all of the required files is a startup error even when it is
writable: completing somebody else's partial set would mint a CA key
that does not match the CA certificate already sitting there. Move the
existing files aside if what you want is a fresh development set.

**`preprovisioned` mode writes nothing at all, ever.** Not a mint into
an empty directory even when that directory is writable, not a
completion of a partial one, not so much as a temporary file. A
read-only mount is therefore a *supported* shape for that mode rather
than one that merely happens to work: mount the volume `:ro` and the
bridge has nothing it wants to do to it.

Writability is decided by attempting a write and removing it again,
not by reading permission bits, so ownership, ACLs and container uid
mapping are all accounted for. A permission error is never mistaken
for a missing file.

### Missing material stops the process

A certificate problem is fatal at startup. The bridge exits non-zero
and does not start degraded. The message names the mode, the
directory, what that mode requires, what was found, what was missing,
and what to do, so it is actionable without reading source:

```
bridge: sep2 embed: sep2embed: server identity: sep2embed: certificate
material is missing and this mode never creates it: mode
"preprovisioned" requires ca.pem, server.pem, server-key.pem in
directory "/etc/sep2/certs"; found (none); missing ca.pem, server.pem,
server-key.pem. This mode never creates certificate material, so it
cannot supply the missing files and did not write anything. Place them
in that directory before starting, or start in dev-mint mode if this
is a development host.
```

A partial directory, here `dev-mint` with the CA key absent, reports
what it found alongside what it did not:

```
... sep2embed: certificate directory is partially populated: mode
"dev-mint" requires ca.pem, ca-key.pem, server.pem, server-key.pem in
directory "/etc/sep2/certs"; found ca.pem, server.pem, server-key.pem;
missing ca-key.pem. Nothing was written. ...
```

The *found* list can name a file the *requires* list does not. Here a
`preprovisioned` directory holds nothing but a stray `ca-key.pem`, left
over from, say, a `dev-mint` directory copied by mistake: every file
this mode actually needs is missing, and the message says so without
also claiming the mode needs the signing key it will never read:

```
... sep2embed: certificate material is missing and this mode never
creates it: mode "preprovisioned" requires ca.pem, server.pem,
server-key.pem in directory "/etc/sep2/certs"; found ca-key.pem;
missing ca.pem, server.pem, server-key.pem. This mode never creates
certificate material, ...
```

A device with no preprovisioned certificate fails the same way, naming
the mRID and the exact path it looked for:

```
bridge: device identities: sep2embed: certificate material is missing
and this mode never creates it: mode "preprovisioned" requires an
operator-supplied certificate for mRID "_1A2B3C4D-..." at
"/etc/sep2/certs/devices/_1A2B3C4D-...-768877d10f057eed.x509", and
none is there. Nothing was written; this mode never mints one (fail
closed)
```

This is a *startup* rule and it stops there. Once the bridge is
serving, a client it cannot serve is refused (a rejected TLS handshake,
or HTTP 403) and the bridge keeps serving every other device. No
client can take the process down.

## Quickest path: let the bridge mint its own

This is the default (`SEP2_DEVICE_CERT_MODE=dev-mint`) and needs no
manual steps. Point `SEP2_SERVER_CERT_DIR` at an empty directory
outside any repository checkout:

```bash
mkdir -p -m 0700 /home/youruser/sep2-certs
export SEP2_SERVER_CERT_DIR=/home/youruser/sep2-certs
```

On first boot, the bridge generates a self-signed **device CA**, a
self-signed **serving CA**, a server leaf certificate signed by the
serving CA, and one device certificate per DER it discovers via CIM,
signed by the device CA. **Give a client `serving-ca.pem`, not
`ca.pem`, as the anchor it uses to verify the bridge**: `ca.pem` only
signs device certificates and cannot verify the server. A client
configured against a pre-#118 single-CA directory that trusted `ca.pem`
for both purposes must be repointed at `serving-ca.pem` the first time
it talks to a freshly minted directory; the bridge's startup log names
both files and this distinction when it mints.

The material persists in that directory across restarts, which
matters: if the directory is deleted or recreated, the bridge mints
fresh CAs and every previously trusted certificate, device and server
alike, stops chaining to them. This material is development-only. Do
not point a production deployment at it.

Dev-mint puts `localhost` and `127.0.0.1` in the server leaf. To add
host names or IPs that clients dial, set `SEP2_SERVER_CERT_HOSTS` to a
comma-separated list before the first start. It is read only when the
directory is empty: an existing `server.pem` is never re-minted, and the
bridge logs a warning when it lacks a requested name. Move the directory
aside to mint a new set (new CAs too).

## A certificate set made on another computer

The bridge runs from a set made elsewhere, mounted read-only, with
`SEP2_DEVICE_CERT_MODE=preprovisioned`. In the container that is
`BRIDGE_CERT_DIR=/path/to/set`, `BRIDGE_CERT_MODE=ro` and
`SEP2_DEVICE_CERT_MODE=preprovisioned` in `.env`. The set needs:

| Path | Content |
|---|---|
| `ca.pem` | Device CA certificate; every device certificate must chain to it. |
| `server.pem`, `server-key.pem` | Server leaf and key. The leaf needs the ServerAuth usage and a SAN naming every address or host name clients dial, such as the LAN IP. |
| `serving-ca.pem` | Optional unless the leaf is signed by a CA other than `ca.pem`; the CA that signed `server.pem`. This is the file clients are given to verify the bridge. |
| `devices/<safe-mrid>-<hash>.x509` | One per device mRID the CIM query returns, raw DER. `<safe-mrid>` is the mRID with every character outside letters, digits, `-` and `_` replaced by `_`, cut to 64 characters; `<hash>` is the first 8 bytes of the SHA-256 of the unmodified mRID as 16 hex digits. |

Write keys as PKCS8 PEM ECDSA, the form core's `sep2cert` produces and
its key parser requires; the device key sits beside its certificate as
`<safe-mrid>-<hash>.pem` (the client side reads it). Leave
`ca-key.pem` and `serving-ca-key.pem` out of the mount: preprovisioned
mode never reads them. Every device mRID without a certificate stops the
start. Device certificates need the `HardwareModuleName` SAN described
below, so make them with core's `sep2cert` (as the test
`foreign_certset_test.go` does) or copy the `devices/` directory from a
dev-mint run on the other computer. The device LFDI is the SHA-256 of the
DER. `go test ./internal/sep2embed -run ForeignCertSet` builds such a set,
mounts it read-only, and completes an mTLS handshake against a LAN address.

## Manual path: generating your own CA and server certificate

Use this if you want to inspect, version, or preprovision the server
identity yourself, for example ahead of a `preprovisioned` deployment.
The bridge's own certificate generation uses ECDSA P-256 keys; the
commands below match that.

This walkthrough signs `server.pem` with the SAME CA that would sign
device certificates, i.e. it does not split. That is a valid choice in
`preprovisioned` mode: the bridge never checks `server.pem`'s issuer
against anything, so a device-CA-only setup here loads and runs exactly
as before #118. If you want the split's actual benefit, that a
device-CA replacement cannot strand the server's own identity, repeat
the CA-generation step under `serving-ca.pem`/`serving-ca-key.pem` with
a different `-subj`, and sign `server.pem` with that pair instead of
`ca.pem`/`ca-key.pem`.

```bash
CERT_DIR=/etc/sep2/certs   # any absolute path outside a repository checkout
mkdir -p -m 0700 "$CERT_DIR"

# CA certificate and key
openssl ecparam -name prime256v1 -genkey -noout -out "$CERT_DIR/ca-key.pem"
openssl req -x509 -new -key "$CERT_DIR/ca-key.pem" -days 3650 \
  -subj "/CN=example-bridge-ca" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -out "$CERT_DIR/ca.pem"

# Server leaf certificate, signed by the CA above
openssl ecparam -name prime256v1 -genkey -noout -out "$CERT_DIR/server-key.pem"
openssl req -new -key "$CERT_DIR/server-key.pem" \
  -subj "/CN=example-bridge-server" -out /tmp/server.csr
openssl x509 -req -in /tmp/server.csr \
  -CA "$CERT_DIR/ca.pem" -CAkey "$CERT_DIR/ca-key.pem" \
  -days 825 -set_serial 1 \
  -extfile <(printf "subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n") \
  -out "$CERT_DIR/server.pem"
rm -f /tmp/server.csr

chmod 0600 "$CERT_DIR"/*.pem
```

Point the bridge at it:

```bash
export SEP2_SERVER_CERT_DIR=/etc/sep2/certs
```

The bridge loads what is there and mints nothing.

For a production deployment, do not leave a CA private key alongside
the server key. Generate the CA (or CAs) somewhere else, copy only
`ca.pem`, `server.pem` and `server-key.pem` (and `serving-ca.pem`, if
you split) to the bridge host, and run with
`SEP2_DEVICE_CERT_MODE=preprovisioned`, which requires those files and
never reads `ca-key.pem` or `serving-ca-key.pem`.

## Device certificates in preprovisioned mode

Generic `openssl` cannot produce these. IEEE 2030.5 device
certificates carry a `HardwareModuleName` Subject Alternative Name
extension (see `internal/sep2embed/devicecert.go`,
`verifyDeviceCertChain`), which the bridge's own certificate
verification requires and a plain client certificate will not have.
Preprovisioning devices needs tooling that produces that extension;
none is shipped in this repository today.

The two knobs are independent, so a practical middle path is to
preprovision the server identity above while leaving
`SEP2_DEVICE_CERT_MODE=dev-mint`: the bridge mints device
certificates signed by your own CA. That mode signs, so it needs
`ca-key.pem` present alongside the other three; supply all four or the
bridge refuses to start.

## Where the directory must not be

Not inside this repository. Not inside any other git working tree.
Not inside a directory you might later `git add -A` from. The default
`SEP2_SERVER_CERT_DIR` value, `./sep2-certs`, is relative to wherever
the bridge process is started; running it from a repository root with
no override writes private key material into that checkout. Always
set `SEP2_SERVER_CERT_DIR` to an explicit absolute path outside any
checkout, in every environment, including local development.
