#!/usr/bin/env bash
#
# GAGO-093 mTLS interop / conformance harness.
#
# This script is the SOURCE OF TRUTH that the admin UI /api/clients panel
# (GAGO-092, Maya) is meant to reflect. It drives at least two mTLS clients
# against the running bridge (one VALID device cert that chains to the
# bridge CA, one BAD-CHAIN cert signed by a rogue CA) and asserts, on the
# ACTUAL field values returned by GET /api/clients, that:
#
#   A1  the valid device cert completes an mTLS handshake and is recorded
#       accepted=true in the handshake log;
#   A2  the valid client appears in "clients" with requestCount > 0;
#   A3  the LFDI the observer reports for the valid client is byte-exact
#       with the LFDI independently derived by openssl over the device
#       .x509 DER (spec section 6.3.4: SHA-256 of the DER, first 20 bytes,
#       40 uppercase hex chars). This is the "generated certs work with the
#       CA and the LFDI is real" proof;
#   A4  the bad-chain cert is recorded accepted=false with a real x509
#       chain/authority failure reason (not a wrong-port or unrelated
#       rejection);
#   A5  the bad-chain LFDI is ABSENT from "clients": a rejected handshake
#       never reaches the request path, so it must never be counted as a
#       connected client.
#
# The harness NEVER weakens the bridge's mTLS verification to force a pass.
# A valid cert that cannot connect, or a bad cert that is accepted, is a
# real finding and is reported as a FAIL, not papered over.
#
# It does not touch the docker compose stack or the CIM dataset. It starts
# the bridge binary directly (the bridge is a Go binary; run.sh in the
# gridappsd-docker repo starts the platform stack, which is assumed already
# up) against the live broker and Blazegraph, or fails loudly with the
# blocker if the platform is not reachable.
#
# Usage:
#   harness/mtls-conformance/run_conformance.sh
#
# Environment overrides (all optional; defaults target Craig's live
# Southern dev stack):
#   BRIDGE_REPO        repo root holding cmd/bridge (default: script's repo)
#   STOMP_ADDR         GridAPPS-D broker host:port (default 127.0.0.1:61613)
#   FEEDER_MRID        CIM feeder mRID (default: Southern reduced feeder)
#   SEP2_ADDR          bridge mTLS listener host:port (default 127.0.0.1:8443)
#   ADMIN_ADDR         bridge admin UI host:port (default 127.0.0.1:8444)
#   OUT_DIR            artifact output directory (default: knowledge outputs)
#   CERT_DIR           bridge cert material dir (default: fresh mktemp dir)
#
set -euo pipefail

# --- configuration ---------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BRIDGE_REPO="${BRIDGE_REPO:-$(cd "${SCRIPT_DIR}/../.." && pwd)}"
STOMP_ADDR="${STOMP_ADDR:-127.0.0.1:61613}"
FEEDER_MRID="${FEEDER_MRID:-510950FB-0686-4956-8A2A-636C049FAB3F}"
SEP2_ADDR="${SEP2_ADDR:-127.0.0.1:8443}"
ADMIN_ADDR="${ADMIN_ADDR:-127.0.0.1:8444}"
OUT_DIR="${OUT_DIR:-/home/debian/knowledge/projects/gridappsd-ieee-2030_5-go/artifacts/outputs}"

# Cert material dir. A fresh dir forces the bridge to dev-mint a CA plus a
# device cert per discovered device: exactly the "generated" material this
# harness proves interoperates. Kept out of the repo and removed on exit.
CERT_DIR="${CERT_DIR:-$(mktemp -d /tmp/gago093-certs.XXXXXX)}"

# Admin Bearer token: generated per run, never committed, never logged in
# full. The bridge scrubs it from its own env after reading it.
ADMIN_KEY="$(openssl rand -hex 16)"

# Scratch dir for client cert material and captured transcripts.
WORK_DIR="$(mktemp -d /tmp/gago093-work.XXXXXX)"

BRIDGE_PID=""
BRIDGE_LOG="${WORK_DIR}/bridge.log"

# Transcript and matrix artifact paths.
TS_VALID="${OUT_DIR}/gago-093-valid-client-transcript.txt"
TS_BAD="${OUT_DIR}/gago-093-bad-chain-transcript.txt"
TS_CLIENTS="${OUT_DIR}/gago-093-api-clients.json"
TS_BRIDGE="${OUT_DIR}/gago-093-bridge-startup.log"
MATRIX="${OUT_DIR}/gago-093-conformance-matrix.md"

# Assertion bookkeeping.
declare -a RESULTS=()
FAILED=0

record() {
	# record <id> <PASS|FAIL> <description> <evidence>
	RESULTS+=("$1|$2|$3|$4")
	if [ "$2" = "FAIL" ]; then
		FAILED=1
	fi
	printf '  [%s] %s: %s\n' "$2" "$1" "$3" >&2
}

cleanup() {
	if [ -n "${BRIDGE_PID}" ] && kill -0 "${BRIDGE_PID}" 2>/dev/null; then
		kill "${BRIDGE_PID}" 2>/dev/null || true
		wait "${BRIDGE_PID}" 2>/dev/null || true
	fi
	# Persist the bridge log to the output dir before dropping the scratch.
	if [ -f "${BRIDGE_LOG}" ]; then
		cp "${BRIDGE_LOG}" "${TS_BRIDGE}" 2>/dev/null || true
	fi
	# Cert material and private keys are secret; never leave them behind.
	rm -rf "${CERT_DIR}" "${WORK_DIR}" 2>/dev/null || true
}
trap cleanup EXIT

die() {
	printf 'BLOCKED: %s\n' "$1" >&2
	exit 2
}

# --- preflight: the live platform must actually be up ----------------------

preflight() {
	printf '== preflight ==\n' >&2

	# Broker reachable.
	local host port
	host="${STOMP_ADDR%%:*}"
	port="${STOMP_ADDR##*:}"
	if ! timeout 5 bash -c "cat < /dev/null > /dev/tcp/${host}/${port}" 2>/dev/null; then
		die "GridAPPS-D broker not reachable at ${STOMP_ADDR}; the platform stack must be up before this harness runs"
	fi
	printf '  broker reachable at %s\n' "${STOMP_ADDR}" >&2

	# Bridge mTLS port and admin port must be free (we start our own bridge).
	local sport aport
	sport="${SEP2_ADDR##*:}"
	aport="${ADMIN_ADDR##*:}"
	if ss -ltn 2>/dev/null | grep -q ":${sport} "; then
		die "bridge mTLS port ${sport} already in use; refusing to double-start (stop the other bridge or set SEP2_ADDR)"
	fi
	if ss -ltn 2>/dev/null | grep -q ":${aport} "; then
		die "admin port ${aport} already in use; refusing to double-start (set ADMIN_ADDR)"
	fi

	mkdir -p "${OUT_DIR}"
}

# --- build and start the bridge --------------------------------------------

start_bridge() {
	printf '== build + start bridge ==\n' >&2
	(cd "${BRIDGE_REPO}" && GOPRIVATE='github.com/GRIDAPPSD/*' go build -o "${WORK_DIR}/bridge" ./cmd/bridge) \
		|| die "bridge build failed"

	# Dev stack broker is plaintext; feeder is Southern; admin UI on.
	SEP2_STOMP_ADDR="${STOMP_ADDR}" \
	SEP2_STOMP_ALLOW_PLAINTEXT=true \
	SEP2_FEEDER_MRID="${FEEDER_MRID}" \
	SEP2_SERVER_ADDR="${SEP2_ADDR}" \
	SEP2_SERVER_CERT_DIR="${CERT_DIR}" \
	SEP2_DEVICE_CERT_MODE=dev-mint \
	SEP2_ADMIN_UI_ADDR="${ADMIN_ADDR}" \
	SEP2_ADMIN_UI_KEY="${ADMIN_KEY}" \
		"${WORK_DIR}/bridge" > "${BRIDGE_LOG}" 2>&1 &
	BRIDGE_PID=$!

	# Wait for readiness: admin /api/health reporting a populated registry.
	local health count
	for _ in $(seq 1 60); do
		if ! kill -0 "${BRIDGE_PID}" 2>/dev/null; then
			cat "${BRIDGE_LOG}" >&2 || true
			die "bridge exited during startup (see log above)"
		fi
		health="$(curl -fsS -H "Authorization: Bearer ${ADMIN_KEY}" \
			"http://${ADMIN_ADDR}/api/health" 2>/dev/null || true)"
		if [ -n "${health}" ]; then
			count="$(printf '%s' "${health}" | jq -r '.registryCount // 0')"
			if [ "${count}" -gt 0 ] 2>/dev/null; then
				printf '  bridge up: registryCount=%s feederMrid=%s\n' \
					"${count}" "$(printf '%s' "${health}" | jq -r '.feederMrid')" >&2
				REGISTRY_COUNT="${count}"
				return 0
			fi
		fi
		sleep 1
	done
	cat "${BRIDGE_LOG}" >&2 || true
	die "bridge did not reach a populated-registry ready state within 60s"
}

# --- LFDI derivation (independent openssl cross-check) ----------------------

# lfdi_of_der <der-file>: spec 6.3.4 LFDI = SHA-256 over the DER bytes,
# first 20 bytes, rendered as 40 uppercase hex characters.
lfdi_of_der() {
	openssl dgst -sha256 -r "$1" | cut -c1-40 | tr 'a-f' 'A-F'
}

# --- valid client leg -------------------------------------------------------

drive_valid_client() {
	printf '== valid device client ==\n' >&2

	# The bridge dev-minted one device cert per discovered device under
	# CERT_DIR/devices as raw DER (.x509) plus a sibling PEM key (.pem).
	local x509 key
	x509="$(find "${CERT_DIR}/devices" -maxdepth 1 -name '*.x509' | sort | head -1)"
	if [ -z "${x509}" ]; then
		die "no device .x509 cert found under ${CERT_DIR}/devices; device-cert emission (GAGO-052) did not run"
	fi
	key="${x509%.x509}.pem"
	[ -f "${key}" ] || die "device key ${key} missing beside ${x509}"

	# openssl-derived LFDI over the exact DER bytes the server receives.
	VALID_LFDI="$(lfdi_of_der "${x509}")"
	printf '  valid device cert: %s\n  openssl LFDI: %s\n' "${x509}" "${VALID_LFDI}" >&2

	# curl needs a PEM cert; converting DER->PEM re-encodes the SAME DER
	# bytes (PEM is base64 of the DER), so the server parses back an
	# identical cert and the LFDI is unchanged.
	local cert_pem="${WORK_DIR}/valid-cert.pem"
	openssl x509 -inform DER -in "${x509}" -out "${cert_pem}" 2>/dev/null \
		|| die "failed to convert device cert to PEM"

	# Drive real requests over mTLS. /dcap and /edev twice so requestCount
	# is unambiguously > 1 and multiple paths are recorded.
	{
		printf '### valid client: mTLS GET against %s ###\n' "${SEP2_ADDR}"
		printf '# device cert: %s\n# openssl LFDI: %s\n\n' "${x509}" "${VALID_LFDI}"
		local path
		for path in /dcap /edev /dcap /edev; do
			printf '#### GET %s ####\n' "${path}"
			curl -sS -i \
				--cacert "${CERT_DIR}/ca.pem" \
				--cert "${cert_pem}" \
				--key "${key}" \
				"https://${SEP2_ADDR}${path}" 2>&1 || printf '(curl rc=%s)\n' "$?"
			printf '\n'
		done
	} > "${TS_VALID}"

	# Client-side confirmation the mTLS session actually served content,
	# independent of the server-side observer (asserted separately as A2).
	if grep -q '200 OK' "${TS_VALID}"; then
		printf '  valid client saw HTTP 200 over mTLS\n' >&2
	else
		printf '  WARNING: valid client saw no 200 OK; see %s\n' "${TS_VALID}" >&2
	fi
}

# --- bad-chain client leg ---------------------------------------------------

drive_bad_client() {
	printf '== bad-chain client ==\n' >&2

	# Build a rogue CA and a leaf signed by it. Presenting the leaf alone
	# (rogue CA never sent) gives the bridge no path to its trusted root, so
	# verification fails with a genuine "signed by unknown authority", not a
	# transport or port error. EC P-256 matches the bridge's own key type so
	# the failure is purely about the chain, never a cipher/sig mismatch.
	local rca_key="${WORK_DIR}/rogue-ca-key.pem"
	local rca_crt="${WORK_DIR}/rogue-ca.pem"
	local bad_key="${WORK_DIR}/bad-key.pem"
	local bad_csr="${WORK_DIR}/bad.csr"
	local bad_crt="${WORK_DIR}/bad-cert.pem"
	local bad_der="${WORK_DIR}/bad-cert.der"

	openssl ecparam -name prime256v1 -genkey -noout -out "${rca_key}" 2>/dev/null
	openssl req -x509 -new -key "${rca_key}" -days 1 -subj '/CN=gago093-rogue-ca' \
		-addext 'basicConstraints=critical,CA:TRUE' -out "${rca_crt}" 2>/dev/null

	openssl ecparam -name prime256v1 -genkey -noout -out "${bad_key}" 2>/dev/null
	openssl req -new -key "${bad_key}" -subj '/CN=gago093-rogue-device' -out "${bad_csr}" 2>/dev/null
	openssl x509 -req -in "${bad_csr}" -CA "${rca_crt}" -CAkey "${rca_key}" \
		-days 1 -set_serial 1 \
		-extfile <(printf 'extendedKeyUsage=clientAuth\n') \
		-out "${bad_crt}" 2>/dev/null

	openssl x509 -in "${bad_crt}" -outform DER -out "${bad_der}" 2>/dev/null
	BAD_LFDI="$(lfdi_of_der "${bad_der}")"
	printf '  bad-chain leaf signed by rogue CA; openssl LFDI: %s\n' "${BAD_LFDI}" >&2

	# Present the bad cert. curl is expected to FAIL the handshake: the
	# server rejects the chain and sends a TLS alert. A zero exit here would
	# itself be a finding (bad cert wrongly accepted).
	{
		printf '### bad-chain client: mTLS GET against %s ###\n' "${SEP2_ADDR}"
		printf '# rogue-CA-signed leaf; openssl LFDI: %s\n\n' "${BAD_LFDI}"
		printf '#### GET /dcap (expected: handshake rejected) ####\n'
		set +e
		curl -sS -i -v \
			--cacert "${CERT_DIR}/ca.pem" \
			--cert "${bad_crt}" \
			--key "${bad_key}" \
			"https://${SEP2_ADDR}/dcap" 2>&1
		BAD_CURL_RC=$?
		set -e
		printf '\n(curl rc=%s)\n' "${BAD_CURL_RC}"
	} > "${TS_BAD}"

	printf '  bad-chain curl rc=%s (nonzero = client saw rejection)\n' "${BAD_CURL_RC}" >&2
}

# --- poll the observer and assert on real field values ----------------------

assert_observer() {
	printf '== poll /api/clients and assert ==\n' >&2

	# The observer records synchronously inside the handshake/request path,
	# but poll a few times to be robust against any scheduling lag.
	local clients_json
	for _ in $(seq 1 10); do
		clients_json="$(curl -fsS -H "Authorization: Bearer ${ADMIN_KEY}" \
			"http://${ADMIN_ADDR}/api/clients" 2>/dev/null || true)"
		if [ -n "${clients_json}" ] && \
			printf '%s' "${clients_json}" | jq -e '.clients | length > 0' >/dev/null 2>&1; then
			break
		fi
		sleep 1
	done
	[ -n "${clients_json}" ] || die "GET /api/clients returned nothing"
	printf '%s\n' "${clients_json}" | jq '.' > "${TS_CLIENTS}"

	# A1: valid handshake recorded accepted=true.
	local va
	va="$(printf '%s' "${clients_json}" \
		| jq -r --arg l "${VALID_LFDI}" \
		'[.handshakes[] | select(.lfdi==$l and .accepted==true)] | length')"
	if [ "${va:-0}" -gt 0 ] 2>/dev/null; then
		record A1 PASS "valid cert handshake accepted=true" "lfdi=${VALID_LFDI}"
	else
		record A1 FAIL "valid cert handshake NOT recorded accepted=true" "lfdi=${VALID_LFDI}"
	fi

	# A2: valid client present in clients with requestCount > 0.
	local rc
	rc="$(printf '%s' "${clients_json}" \
		| jq -r --arg l "${VALID_LFDI}" \
		'[.clients[] | select(.lfdi==$l)] | first | .requestCount // 0')"
	if [ "${rc:-0}" -gt 0 ] 2>/dev/null; then
		record A2 PASS "valid client in clients with requestCount>0" "requestCount=${rc}"
	else
		record A2 FAIL "valid client absent or requestCount==0" "requestCount=${rc:-none}"
	fi

	# A3: observer-reported LFDI byte-exact with openssl-derived LFDI.
	local reported
	reported="$(printf '%s' "${clients_json}" \
		| jq -r --arg l "${VALID_LFDI}" \
		'.clients[] | select(.lfdi==$l) | .lfdi' | head -1)"
	if [ "${reported}" = "${VALID_LFDI}" ] && [ -n "${reported}" ]; then
		record A3 PASS "observer LFDI byte-exact with openssl(cert DER)" "openssl=${VALID_LFDI} observer=${reported}"
	else
		record A3 FAIL "observer LFDI does not match openssl-derived LFDI" "openssl=${VALID_LFDI} observer=${reported:-none}"
	fi

	# A4: bad-chain handshake recorded accepted=false with a chain/authority reason.
	local reason
	reason="$(printf '%s' "${clients_json}" \
		| jq -r '[.handshakes[] | select(.accepted==false)][0].reason // ""')"
	BAD_REASON="${reason}"
	if printf '%s' "${reason}" | grep -qiE 'unknown authority|signed by unknown|x509'; then
		record A4 PASS "bad-chain recorded accepted=false with x509 chain failure" "reason=${reason}"
	else
		record A4 FAIL "no accepted=false handshake with a chain/authority reason" "reason=${reason:-none}"
	fi

	# A5: bad-chain LFDI absent from clients.
	local present
	present="$(printf '%s' "${clients_json}" \
		| jq -r --arg l "${BAD_LFDI}" \
		'[.clients[] | select(.lfdi==$l)] | length')"
	if [ "${present:-0}" -eq 0 ] 2>/dev/null; then
		record A5 PASS "bad-chain LFDI absent from clients" "lfdi=${BAD_LFDI}"
	else
		record A5 FAIL "bad-chain LFDI wrongly present in clients" "lfdi=${BAD_LFDI}"
	fi
}

# --- matrix artifact --------------------------------------------------------

write_matrix() {
	local verdict="PASS"
	[ "${FAILED}" -eq 0 ] || verdict="FAIL"
	{
		printf '# GAGO-093 mTLS interop / conformance matrix\n\n'
		printf 'Date: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		printf 'Engineer: Devi (interoperability / conformance)\n'
		printf 'Verdict: %s\n\n' "${verdict}"
		printf '## Environment\n\n'
		printf -- '- Bridge built from the GAGO-093 worktree (stacked on Kira'\''s feat/gago-090-connection-observer).\n'
		printf -- '- Live platform: gridappsd-docker Southern stack. Feeder mRID %s, registry populated with %s devices.\n' \
			"${FEEDER_MRID}" "${REGISTRY_COUNT:-?}"
		printf -- '- Broker %s (plaintext dev stack). mTLS listener %s. Admin UI %s.\n' \
			"${STOMP_ADDR}" "${SEP2_ADDR}" "${ADMIN_ADDR}"
		printf -- '- Cert material dev-minted into a fresh dir: a self-signed CA plus one device cert per device (GAGO-052). The valid client presents a bridge-emitted device cert; the bad-chain client presents a leaf signed by a rogue CA the bridge does not trust.\n\n'
		printf '## Observed identities\n\n'
		printf -- '- Valid device LFDI (openssl over the .x509 DER, spec 6.3.4): `%s`\n' "${VALID_LFDI}"
		printf -- '- Bad-chain leaf LFDI (openssl): `%s`\n' "${BAD_LFDI}"
		printf -- '- Bad-chain rejection reason (verifier'\''s own string): `%s`\n\n' "${BAD_REASON}"
		printf '## Conformance matrix: assertion -> result\n\n'
		printf '| # | Assertion | Result | Evidence |\n'
		printf '|---|---|---|---|\n'
		local r id res desc ev
		for r in "${RESULTS[@]}"; do
			IFS='|' read -r id res desc ev <<< "${r}"
			printf '| %s | %s | %s | %s |\n' "${id}" "${desc}" "${res}" "${ev}"
		done
		printf '\n## Method\n\n'
		printf 'The bridge is the unit under test and its mTLS verification was NOT weakened. '
		printf 'The valid leg drives real GET /dcap and /edev requests over mTLS with a bridge-emitted device cert; '
		printf 'the bad leg presents a rogue-CA-signed leaf and is expected to fail the handshake. '
		printf 'Every assertion is on the actual field values GET /api/clients returns, cross-checked against an '
		printf 'independent openssl LFDI derivation, per the data-invariants rule (assert values, not non-crash).\n\n'
		printf '## Artifacts\n\n'
		printf -- '- Valid-client transcript: `%s`\n' "${TS_VALID}"
		printf -- '- Bad-chain transcript: `%s`\n' "${TS_BAD}"
		printf -- '- Raw /api/clients JSON: `%s`\n' "${TS_CLIENTS}"
		printf -- '- Bridge startup log: `%s`\n' "${TS_BRIDGE}"
	} > "${MATRIX}"
	printf '\n== matrix written: %s (verdict %s) ==\n' "${MATRIX}" "${verdict}" >&2
}

# --- main -------------------------------------------------------------------

preflight
start_bridge
drive_valid_client
drive_bad_client
assert_observer
write_matrix

if [ "${FAILED}" -eq 0 ]; then
	printf '\nCONFORMANCE: PASS\n' >&2
	exit 0
fi
printf '\nCONFORMANCE: FAIL (see matrix)\n' >&2
exit 1
