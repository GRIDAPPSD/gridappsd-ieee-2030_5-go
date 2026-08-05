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
the process's working directory) must hold exactly four fixed file
names:

| File | Purpose |
|---|---|
| `ca.pem` | The CA certificate, the trust anchor every device and client cert chains to. |
| `ca-key.pem` | The CA's private key. Needed only to sign new certificates; a server that only loads preprovisioned material never needs to read it. |
| `server.pem` | The embedded server's own leaf certificate. |
| `server-key.pem` | The embedded server's private key. |

The directory is created at mode `0700` and each file at mode `0600`
when the bridge writes them. Match those permissions if you supply
your own.

**Current behavior is all-or-nothing**: if any one of the four files
is missing, the bridge mints a fresh, self-signed development CA and
server certificate, writes all four files, and logs a loud warning.
There is no partial state. This all-or-nothing check is under active
revision as of this writing, specifically around whether
`preprovisioned` mode should require `ca-key.pem` at all; treat the
exact trigger condition as subject to change and re-check
`internal/sep2embed/certs.go` if this section and the code appear to
disagree.

Per-device certificates, used when `SEP2_DEVICE_CERT_MODE=preprovisioned`,
live under `<SEP2_SERVER_CERT_DIR>/devices/`, one raw DER-encoded
`<name>.x509` file per device, signed by the same CA as the server
identity above.

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

If `ca.pem`, `ca-key.pem`, `server.pem`, and `server-key.pem` are all
present, the bridge loads them as-is and mints nothing.

For a genuinely production deployment, an operator normally does not
want the CA private key sitting alongside the server key at all;
withhold `ca-key.pem` only once you have confirmed, against the
current behavior of `ensureServerIdentity` in
`internal/sep2embed/certs.go`, that your build's all-or-nothing check
accepts that. See the note above: this is one of the things in
flight.

## Device certificates in preprovisioned mode

Generic `openssl` cannot produce these. IEEE 2030.5 device
certificates carry a `HardwareModuleName` Subject Alternative Name
extension (see `internal/sep2embed/devicecert.go`,
`verifyDeviceCertChain`), which the bridge's own certificate
verification requires and a plain client certificate will not have.
Preprovisioning devices needs tooling that produces that extension;
none is shipped in this repository today.

The two knobs are independent, so a practical middle path is to
preprovision the server identity above (`ca.pem`, `server.pem`,
`server-key.pem`) while leaving `SEP2_DEVICE_CERT_MODE=dev-mint`: the
bridge mints device certificates signed by your own preprovisioned
CA. This requires `ca-key.pem` to be present, since minting needs the
signing key.

## Where the directory must not be

Not inside this repository. Not inside any other git working tree.
Not inside a directory you might later `git add -A` from. The default
`SEP2_SERVER_CERT_DIR` value, `./sep2-certs`, is relative to wherever
the bridge process is started; running it from a repository root with
no override writes private key material into that checkout. Always
set `SEP2_SERVER_CERT_DIR` to an explicit absolute path outside any
checkout, in every environment, including local development.
