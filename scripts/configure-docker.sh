#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "configure-docker: $*" >&2
  exit 1
}

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
env_file="${BRIDGE_ENV_FILE:-$repo/.env}"
template="${BRIDGE_ENV_TEMPLATE:-$repo/.env.example}"

[ -r "$template" ] || die "env template not found or unreadable: $template"
[ ! -e "$env_file" ] || die "env file already exists: $env_file (edit it directly; it was not changed)"

interactive=0
if [ -r /dev/tty ] && [ "${BRIDGE_CONFIGURE_NONINTERACTIVE:-0}" != 1 ]; then
  interactive=1
fi

broker_password="${GRIDAPPSD_PASSWORD:-${SEP2_STOMP_PASSWORD:-}}"
if [ -z "$broker_password" ] && [ "$interactive" -eq 1 ]; then
  printf 'GridAPPS-D broker password [manager]: ' >&2
  IFS= read -r -s broker_password </dev/tty || die "could not read broker password"
  printf '\n' >/dev/tty
fi
broker_password="${broker_password:-manager}"
[[ "$broker_password" != *"'"* && ! "$broker_password" =~ [[:space:]]# ]] ||
  die "broker password cannot contain a single quote or whitespace followed by #"

feeder_default="$(awk -F= '/^SEP2_FEEDER_MRID=/ { print $2; exit }' "$template")"
[ -n "$feeder_default" ] || die "env template has no SEP2_FEEDER_MRID default"
feeder_mrid="${SEP2_FEEDER_MRID:-$feeder_default}"
feeder_name=""
if [ -z "${SEP2_FEEDER_MRID:-}" ] && [ "$interactive" -eq 1 ]; then
  command -v curl >/dev/null 2>&1 || die "curl is required to list feeders from Blazegraph"
  command -v jq >/dev/null 2>&1 || die "jq is required to parse the Blazegraph feeder list"
  sparql_url="${BLAZEGRAPH_SPARQL_URL:-http://127.0.0.1:8889/bigdata/namespace/kb/sparql}"
  feeder_query='PREFIX c: <http://iec.ch/TC57/CIM100#> SELECT DISTINCT ?mrid ?name WHERE { ?fdr a c:Feeder ; c:IdentifiedObject.mRID ?mrid . OPTIONAL { ?fdr c:IdentifiedObject.name ?name } } ORDER BY ?name ?mrid'
  feeder_json="$(curl --fail --silent --show-error --get "$sparql_url" \
    --data-urlencode "query=$feeder_query" \
    --header 'Accept: application/sparql-results+json')" ||
    die "could not query Blazegraph at $sparql_url; start the platform or set BLAZEGRAPH_SPARQL_URL"
  jq -e '(.results.bindings | type == "array" and length > 0)' <<<"$feeder_json" >/dev/null ||
    die "Blazegraph returned no feeder records; check the selected namespace/model"
  mapfile -t feeder_rows < <(jq -r '.results.bindings[] | [.mrid.value, ((.name.value // "(unnamed)") | gsub("[\\t\\r\\n]"; " "))] | @tsv' <<<"$feeder_json")
  default_feeder_id="${feeder_default#_}"
  default_feeder_choice=1
  for index in "${!feeder_rows[@]}"; do
    IFS=$'\t' read -r candidate_id candidate_name <<<"${feeder_rows[$index]}"
    printf '  %d) %s [%s]\n' "$((index + 1))" "$candidate_name" "$candidate_id" >/dev/tty
    if [ "$candidate_id" = "$default_feeder_id" ]; then
      default_feeder_choice="$((index + 1))"
    fi
  done
  printf 'Choose feeder number [%s]: ' "$default_feeder_choice" >/dev/tty
  IFS= read -r feeder_choice </dev/tty || die "could not read feeder selection"
  feeder_choice="${feeder_choice:-$default_feeder_choice}"
  [[ "$feeder_choice" =~ ^[0-9]+$ ]] &&
    [ "$feeder_choice" -ge 1 ] && [ "$feeder_choice" -le "${#feeder_rows[@]}" ] ||
    die "feeder selection must be a number from 1 to ${#feeder_rows[@]}"
  IFS=$'\t' read -r feeder_mrid feeder_name <<<"${feeder_rows[$((feeder_choice - 1))]}"
fi

validate_ipv4() {
  local label="$1" address="$2" octet
  local -a octets
  [[ "$address" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die "$label must be an IPv4 address, got: $address"
  IFS=. read -r -a octets <<<"$address"
  for octet in "${octets[@]}"; do
    { [ "$octet" -le 255 ] && { [ "${#octet}" -eq 1 ] || [ "${octet:0:1}" != 0 ]; }; } ||
      die "$label must be an IPv4 address, got: $address"
  done
}

sep2_bind_ip="${BRIDGE_SEP2_BIND_IP:-}"
if [ -z "$sep2_bind_ip" ] && [ "$interactive" -eq 1 ]; then
  printf 'SEP2 protocol host bind IPv4 [0.0.0.0]: ' >&2
  IFS= read -r sep2_bind_ip </dev/tty || die "could not read SEP2 bind address"
fi
sep2_bind_ip="${sep2_bind_ip:-0.0.0.0}"
validate_ipv4 BRIDGE_SEP2_BIND_IP "$sep2_bind_ip"

admin_bind_ip="${BRIDGE_ADMIN_BIND_IP:-}"
if [ -z "$admin_bind_ip" ]; then
  virtualization="$(systemd-detect-virt --vm 2>/dev/null || true)"
  case "$virtualization" in
    microsoft | hyperv | oracle | virtualbox)
      route="$(ip -4 route get 1.1.1.1 2>/dev/null || true)"
      admin_bind_ip="$(awk '{ for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit } }' <<<"$route")"
      if [ -z "$admin_bind_ip" ]; then
        echo "configure-docker: warning: could not detect the virtual-machine guest IP; keeping Caddy on localhost. Set BRIDGE_ADMIN_BIND_IP to the guest IP for host forwarding." >&2
      fi
      ;;
  esac
fi
admin_bind_ip="${admin_bind_ip:-127.0.0.1}"
if [ -z "${BRIDGE_ADMIN_BIND_IP:-}" ] && [ "$interactive" -eq 1 ]; then
  printf 'Caddy admin host bind IPv4 [%s]: ' "$admin_bind_ip" >&2
  IFS= read -r entered_admin_bind_ip </dev/tty || die "could not read admin bind address"
  admin_bind_ip="${entered_admin_bind_ip:-$admin_bind_ip}"
fi
validate_ipv4 BRIDGE_ADMIN_BIND_IP "$admin_bind_ip"

default_cert_dir="${HOME:?HOME is not set}/.config/gridappsd/2030.5server/sep2-certs"
cert_dir="${BRIDGE_CERT_DIR:-}"
if [ -z "$cert_dir" ] && [ "$interactive" -eq 1 ]; then
  printf 'SEP2 TLS certificate directory [~/.config/gridappsd/2030.5server/sep2-certs]: ' >&2
  IFS= read -r cert_dir </dev/tty || die "could not read certificate directory"
fi
cert_dir="${cert_dir:-$default_cert_dir}"
case "$cert_dir" in
  "~") cert_dir="$HOME" ;;
  "~/"*) cert_dir="$HOME/${cert_dir:2}" ;;
esac
[[ "$cert_dir" = /* ]] || cert_dir="$PWD/$cert_dir"
[[ "$cert_dir" != *"'"* && "$cert_dir" != *$'\n'* && ! "$cert_dir" =~ [[:space:]]# ]] ||
  die "certificate directory cannot contain a single quote or whitespace followed by #"
bridge_user="${BRIDGE_USER:-$(id -u):$(id -g)}"
[[ "$bridge_user" =~ ^[0-9]+:[0-9]+$ ]] || die "BRIDGE_USER must be uid:gid, got: $bridge_user"

command -v openssl >/dev/null 2>&1 || die "openssl is required to generate the admin UI key"
admin_key="$(openssl rand -hex 24)"

env_dir="$(dirname "$env_file")"
mkdir -p -- "$env_dir"
mkdir -p -m 700 -- "$cert_dir"
umask 077
tmp_file="$(mktemp "$env_file.tmp.XXXXXX")"
trap 'rm -f -- "$tmp_file"' EXIT

seen_admin_key=0
seen_broker_password=0
seen_feeder=0
seen_sep2_bind_ip=0
seen_admin_bind_ip=0
seen_bridge_user=0
seen_cert_dir=0
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    SEP2_ADMIN_UI_KEY=)
      printf 'SEP2_ADMIN_UI_KEY=%s\n' "$admin_key" >>"$tmp_file"
      seen_admin_key=1
      ;;
    GRIDAPPSD_PASSWORD=)
      printf "GRIDAPPSD_PASSWORD='%s'\n" "$broker_password" >>"$tmp_file"
      seen_broker_password=1
      ;;
    SEP2_FEEDER_MRID=*)
      printf 'SEP2_FEEDER_MRID=%s\n' "$feeder_mrid" >>"$tmp_file"
      seen_feeder=1
      ;;
    '# BRIDGE_SEP2_BIND_IP=0.0.0.0')
      printf 'BRIDGE_SEP2_BIND_IP=%s\n' "$sep2_bind_ip" >>"$tmp_file"
      seen_sep2_bind_ip=1
      ;;
    '# BRIDGE_ADMIN_BIND_IP=127.0.0.1')
      printf 'BRIDGE_ADMIN_BIND_IP=%s\n' "$admin_bind_ip" >>"$tmp_file"
      seen_admin_bind_ip=1
      ;;
    '# BRIDGE_USER=1000:1000')
      printf 'BRIDGE_USER=%s\n' "$bridge_user" >>"$tmp_file"
      seen_bridge_user=1
      ;;
    '# BRIDGE_CERT_DIR='*)
      printf "BRIDGE_CERT_DIR='%s'\n" "$cert_dir" >>"$tmp_file"
      seen_cert_dir=1
      ;;
    *) printf '%s\n' "$line" >>"$tmp_file" ;;
  esac
done <"$template"

[[ "$seen_admin_key$seen_broker_password$seen_feeder$seen_sep2_bind_ip$seen_admin_bind_ip$seen_bridge_user$seen_cert_dir" == 1111111 ]] ||
  die "env template is missing a required setting; no env file was created"

if ! ln -- "$tmp_file" "$env_file"; then
  die "could not create $env_file (it may already exist)"
fi
rm -f -- "$tmp_file"
trap - EXIT

echo "Created $env_file with mode 600."
echo "Admin UI key generated; retrieve it with: grep '^SEP2_ADMIN_UI_KEY=' '$env_file'"
if [ -n "$feeder_name" ]; then
  echo "Selected feeder: $feeder_name ($feeder_mrid)."
fi
if [ "$admin_bind_ip" = 127.0.0.1 ]; then
  echo "Admin UI is bound to localhost only. Set BRIDGE_ADMIN_BIND_IP to the guest private-switch IP if Windows needs to forward to this guest."
else
  echo "Caddy is bound to $admin_bind_ip; keep the Windows portproxy listener bound to 127.0.0.1."
fi
echo "Start the bridge with: make docker-up"