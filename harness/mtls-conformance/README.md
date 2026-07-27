# GAGO-093 mTLS interop / conformance harness

A scripted interop harness that is the SOURCE OF TRUTH the admin UI
`/api/clients` panel (GAGO-092) reflects. It drives multiple mTLS clients
against the running bridge and asserts that the connection observer's state
matches the expected connected and rejected LFDIs on the wire.

This is a test harness, not a shipped bridge component. It lives in the
worktree only and is not a `go.mod` dependency of the bridge.

## What it proves

Two operator goals, verified on real bytes rather than asserted:

1. Clients are connected. A device presenting a bridge-emitted device cert
   that chains to the bridge CA completes an mTLS handshake and shows up in
   `/api/clients` `clients` with a real, cert-derived LFDI and an
   incrementing `requestCount`.
2. The generated certs work with the CA, and the LFDI is real. The LFDI the
   observer reports is byte-exact with the LFDI derived independently by
   `openssl` over the device `.x509` DER (spec section 6.3.4: SHA-256 of the
   DER, first 20 bytes, 40 uppercase hex characters).

It also proves the negative: a leaf signed by a rogue CA the bridge does not
trust is rejected at the handshake, recorded `accepted=false` with the
verifier's own `x509: certificate signed by unknown authority` reason, and
never appears in `clients`.

## Assertions

| # | Assertion |
|---|---|
| A1 | valid cert handshake recorded `accepted=true` |
| A2 | valid client present in `clients` with `requestCount > 0` |
| A3 | observer-reported LFDI byte-exact with `openssl`-derived LFDI |
| A4 | bad-chain recorded `accepted=false` with an authority-specific chain-verification failure reason (not any x509 error) |
| A5 | bad-chain LFDI absent from `clients` |

## Invariants

- The bridge's mTLS verification is NEVER weakened to force a pass. A valid
  cert that cannot connect, or a bad cert that is accepted, is a real
  finding reported as a FAIL, not worked around with `InsecureSkipVerify`.
- Every assertion is on an actual field value returned by `/api/clients`,
  cross-checked against an independent `openssl` derivation, per the
  data-invariants rule (assert values, not non-crash).
- The harness does not touch the docker compose stack or the CIM dataset.
  It starts the bridge binary directly against an already-running platform,
  and refuses to double-start if the mTLS or admin port is already bound.

## Run

The gridappsd-docker platform stack must already be up (broker plus a
Blazegraph loaded with the target feeder). Then:

```
harness/mtls-conformance/run_conformance.sh
```

The script builds the bridge, starts it with a fresh dev-minted cert dir and
the admin UI enabled, drives the two client legs, polls `/api/clients`, and
writes a conformance matrix plus transcripts to `OUT_DIR` (`gago-093-*`),
which defaults to `artifacts/outputs/` at the repo root (repo-relative, so
the harness is portable to any checkout). Exit 0 = PASS, 1 = one or more
assertions FAILED, 2 = BLOCKED (platform not reachable, or a port already
in use).

Environment overrides (`STOMP_ADDR`, `FEEDER_MRID`, `SEP2_ADDR`,
`ADMIN_ADDR`, `OUT_DIR`, `CERT_DIR`) are documented at the top of the script;
the defaults (other than `OUT_DIR`) target the live Southern dev stack.

## Cert material and secrets

Cert material (including device private keys) is dev-minted into a throwaway
`mktemp` directory and removed on exit. The admin Bearer token is generated
per run with `openssl rand` and never committed. Nothing secret is written to
the repo or left behind.
