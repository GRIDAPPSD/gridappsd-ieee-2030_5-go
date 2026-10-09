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

broker_password="${GRIDAPPSD_PASSWORD:-${SEP2_STOMP_PASSWORD:-}}"
if [ -z "$broker_password" ] && [ -r /dev/tty ]; then
  printf 'GridAPPS-D broker password [manager]: ' >&2
  IFS= read -r -s broker_password </dev/tty || die "could not read broker password"
  printf '\n' >/dev/tty
fi
broker_password="${broker_password:-manager}"
[[ "$broker_password" != *"'"* && ! "$broker_password" =~ [[:space:]]# ]] ||
  die "broker password cannot contain a single quote or whitespace followed by #"

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
[[ "$admin_bind_ip" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die "admin bind address must be an IPv4 address, got: $admin_bind_ip"
IFS=. read -r -a octets <<<"$admin_bind_ip"
for octet in "${octets[@]}"; do
  { [ "$octet" -le 255 ] && { [ "${#octet}" -eq 1 ] || [ "${octet:0:1}" != 0 ]; }; } ||
    die "admin bind address must be an IPv4 address, got: $admin_bind_ip"
done

cert_dir="${BRIDGE_CERT_DIR:-${HOME:?HOME is not set}/.config/gridappsd/2030.5server/sep2-certs}"
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

[[ "$seen_admin_key$seen_broker_password$seen_admin_bind_ip$seen_bridge_user$seen_cert_dir" == 11111 ]] ||
  die "env template is missing a required setting; no env file was created"

if ! ln -- "$tmp_file" "$env_file"; then
  die "could not create $env_file (it may already exist)"
fi
rm -f -- "$tmp_file"
trap - EXIT

echo "Created $env_file with mode 600."
echo "Admin UI key generated; retrieve it with: grep '^SEP2_ADMIN_UI_KEY=' '$env_file'"
if [ "$admin_bind_ip" = 127.0.0.1 ]; then
  echo "Admin UI is bound to localhost only. Set BRIDGE_ADMIN_BIND_IP to the guest private-switch IP if Windows needs to forward to this guest."
else
  echo "Caddy is bound to $admin_bind_ip; keep the Windows portproxy listener bound to 127.0.0.1."
fi
echo "Start the bridge with: make docker-up"