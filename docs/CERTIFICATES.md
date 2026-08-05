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
the four it is.

## What the server identity needs

`SEP2_SERVER_CERT_DIR` (default `./sep2-certs`, resolved relative to
the process's working directory) holds four fixed file names:

| File | Purpose |
|---|---|
| `ca.pem` | The CA certificate, the trust anchor every device and client cert chains to. |
| `ca-key.pem` | The CA's private key. Needed only to sign new certificates. |
| `server.pem` | The embedded server's own leaf certificate. |
| `server-key.pem` | The embedded server's private key. |

Which of them are *required* depends on `SEP2_DEVICE_CERT_MODE`,
because only one of the two modes signs anything:

| Mode | Signs? | Required files |
|---|---|---|
| `dev-mint` | yes | all four |
| `preprovisioned` | no | `ca.pem`, `server.pem`, `server-key.pem` |

**`preprovisioned` does not require `ca-key.pem` and never reads it.**
Keep the CA signing key off the bridge host entirely; issue
certificates wherever you keep it, and copy only the public
certificate across.

The directory is created at mode `0700` and each file at mode `0600`
when the bridge writes them. Match those permissions if you supply
your own.

Per-device certificates, used when `SEP2_DEVICE_CERT_MODE=preprovisioned`,
live under `<SEP2_SERVER_CERT_DIR>/devices/`, one raw DER-encoded
`<name>.x509` file per device, signed by the same CA as the server
identity above.

## What the bridge does at startup

It inspects the directory on every start and does exactly one of:

- **Complete for the mode**: loads the files and writes nothing. The
  directory may be read-only.
- **Empty, in `dev-mint`, and writable**: mints a self-signed
  development CA and server certificate, writes all four files, and
  logs a loud warning.
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

On first boot, the bridge generates a self-signed CA, a server leaf
certificate, and one device certificate per DER it discovers via CIM,
all signed by that same CA. The material persists in that directory
across restarts, which matters: if the directory is deleted or
recreated, the bridge mints a new CA and every previously trusted
device certificate stops chaining to it. This material is
development-only. Do not point a production deployment at it.

## Manual path: generating your own CA and server certificate

Use this if you want to inspect, version, or preprovision the server
identity yourself, for example ahead of a `preprovisioned` deployment.
The bridge's own certificate generation uses ECDSA P-256 keys; the
commands below match that.

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

For a production deployment, do not leave the CA private key alongside
the server key. Generate the CA somewhere else, copy only `ca.pem`,
`server.pem` and `server-key.pem` to the bridge host, and run with
`SEP2_DEVICE_CERT_MODE=preprovisioned`, which requires exactly those
three and never reads the fourth.

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
