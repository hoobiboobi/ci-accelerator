#!/usr/bin/env bash
# Create a CA, a server certificate for the buildkit daemons and client
# certificates for CI jobs, for the chart's tls.existingSecret.
#
#   scripts/buildkit-certs.sh --name bk-buildkit --namespace ci-accelerator
#   scripts/buildkit-certs.sh --name bk-buildkit --namespace ci-accelerator \
#     --no-server --client runner      # one more client from the same CA
#   scripts/buildkit-certs.sh --name bk-buildkit --namespace ci-accelerator \
#     --controller bk                  # the controller API's server certificate
#   scripts/buildkit-certs.sh --name bk-buildkit --namespace ci-accelerator \
#     --router bk                      # daemons reached through the build router
#
# --name is the chart's full name (<release>-buildkit, or the release name
# when it already contains "buildkit"); the chart prints it when
# tls.existingSecret is missing. The server certificate covers the Service
# names clients use; add more with --san (DNS:name or IP:address), and the
# build router's Service names with --router <prefix> (the control plane's
# fullnameOverride, or the release name): jobs reach the daemons through it
# and check its name.
#
# Output (private keys mode 0600, directory 0700):
#   <out>/ca/ca.crt, ca.key       the CA; keep ca.key offline, it signs clients
#   <out>/server/                 tls.crt, tls.key, ca.crt for the daemons
#   <out>/<client>/               tls.crt, tls.key, ca.crt for buildctl --tlsdir
#   <out>/controller-tls/         tls.crt, tls.key, ca.crt for the control plane's
#                                 controller.api.tls.existingSecret (with
#                                 --controller <prefix>: its fullnameOverride,
#                                 or the release name)
# An existing CA in <out>/ca is reused; certificates are reissued.
#
# Any certificate signed by this CA can control the daemons: use the CA for
# nothing else. buildkitd reads its certificate only at start; restart the
# daemons after replacing the Secret.
#
# Needs bash >= 3.2 and openssl (OpenSSL 3 or LibreSSL).

set -euo pipefail

usage() { sed -n '2,35p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
die() { echo "buildkit-certs: $*" >&2; exit 1; }

NAME=""
NAMESPACE=""
OUT=./buildkit-certs
CLUSTER_DOMAIN=cluster.local
CLIENT=client
SERVER=1
CA_DAYS=3650
DAYS=365
EXTRA_SANS=""
CONTROLLER=""
ROUTER=""

# A SAN such as DNS:*.example.com is a name, never a file pattern.
set -f

while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME=${2:?--name needs a value}; shift 2 ;;
    --namespace) NAMESPACE=${2:?--namespace needs a value}; shift 2 ;;
    --out) OUT=${2:?--out needs a value}; shift 2 ;;
    --cluster-domain) CLUSTER_DOMAIN=${2:?--cluster-domain needs a value}; shift 2 ;;
    --san) EXTRA_SANS="$EXTRA_SANS ${2:?--san needs a value}"; shift 2 ;;
    --client) CLIENT=${2:?--client needs a value}; shift 2 ;;
    --no-server) SERVER=0; shift ;;
    --controller) CONTROLLER=${2:?--controller needs a value}; shift 2 ;;
    --router) ROUTER=${2:?--router needs a value}; shift 2 ;;
    --ca-days) CA_DAYS=${2:?--ca-days needs a value}; shift 2 ;;
    --days) DAYS=${2:?--days needs a value}; shift 2 ;;
    -h|--help) usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 2 ;;
  esac
done

dns_label='^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'
[ -n "$NAME" ] || die "--name is required (the chart's full name, e.g. bk-buildkit)"
printf '%s' "$NAME" | grep -Eq "$dns_label" || die "--name: not a DNS label: $NAME"
if [ "$SERVER" = 1 ]; then
  [ -n "$NAMESPACE" ] || die "--namespace is required for the server certificate"
  printf '%s' "$NAMESPACE" | grep -Eq "$dns_label" || die "--namespace: not a DNS label: $NAMESPACE"
fi
printf '%s' "$CLIENT" | grep -Eq "$dns_label" || die "--client: not a DNS label: $CLIENT"
if [ -n "$CONTROLLER" ]; then
  # Only the controller API's certificate: no daemon certificate, no client.
  printf '%s' "$CONTROLLER" | grep -Eq "$dns_label" || die "--controller: not a DNS label: $CONTROLLER"
  [ -n "$NAMESPACE" ] || die "--namespace is required for the controller API's certificate"
  printf '%s' "$NAMESPACE" | grep -Eq "$dns_label" || die "--namespace: not a DNS label: $NAMESPACE"
  SERVER=0
fi
if [ -n "$ROUTER" ]; then
  printf '%s' "$ROUTER" | grep -Eq "$dns_label" || die "--router: not a DNS label: $ROUTER"
  # The control plane's rule: <prefix> cut to 40 characters, then "-router".
  router=${ROUTER:0:40}; router=${router%-}-router
  for suffix in "" ".$NAMESPACE" ".$NAMESPACE.svc" ".$NAMESPACE.svc.$CLUSTER_DOMAIN"; do
    EXTRA_SANS="$EXTRA_SANS DNS:$router$suffix"
  done
fi
[ "$CLIENT" != ca ] && [ "$CLIENT" != server ] || die "--client: '$CLIENT' is reserved"
case "$CA_DAYS$DAYS" in *[!0-9]*) die "--ca-days and --days take a number of days" ;; esac
for san in $EXTRA_SANS; do
  case "$san" in DNS:?*|IP:?*) ;; *) die "--san: expected DNS:<name> or IP:<address>, got '$san'" ;; esac
done
command -v openssl >/dev/null 2>&1 || die "openssl is not installed"

umask 077
mkdir -p "$OUT/ca"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/buildkit-certs.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

# Extensions for every certificate this script issues. ECDSA keys need only
# digitalSignature; the extended key usage is what TLS checks.
cat >"$WORK/ext.cnf" <<'EOF'
[req]
distinguished_name = dn
[dn]
[ca]
basicConstraints = critical, CA:TRUE, pathlen:0
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
[server]
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
subjectAltName = @alt
[client]
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = clientAuth
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
[alt]
EOF

newkey() { openssl ecparam -name prime256v1 -genkey -noout -out "$1" 2>/dev/null || die "cannot create key $1"; }
serial() { echo "0x$(openssl rand -hex 16)"; }

CA_CRT=$OUT/ca/ca.crt
CA_KEY=$OUT/ca/ca.key
if [ -f "$CA_CRT" ] && [ -f "$CA_KEY" ]; then
  echo "reusing CA $CA_CRT"
else
  newkey "$CA_KEY"
  openssl req -x509 -new -key "$CA_KEY" -sha256 -days "$CA_DAYS" \
    -subj "/CN=${NAME} buildkit CA" -set_serial "$(serial)" \
    -config "$WORK/ext.cnf" -extensions ca -out "$CA_CRT" 2>/dev/null || die "cannot create the CA"
  echo "created CA $CA_CRT (valid $CA_DAYS days)"
fi

# issue <dir> <common name> <extension section>
issue() {
  local dir=$1 cn=$2 ext=$3
  mkdir -p "$dir"
  newkey "$dir/tls.key"
  openssl req -new -key "$dir/tls.key" -subj "/CN=$cn" -config "$WORK/ext.cnf" -out "$WORK/req.csr" 2>/dev/null \
    || die "cannot create a request for $cn"
  openssl x509 -req -in "$WORK/req.csr" -CA "$CA_CRT" -CAkey "$CA_KEY" -set_serial "$(serial)" \
    -sha256 -days "$DAYS" -extfile "$WORK/ext.cnf" -extensions "$ext" -out "$dir/tls.crt" 2>/dev/null \
    || die "cannot sign the certificate for $cn"
  cp "$CA_CRT" "$dir/ca.crt"
  openssl verify -CAfile "$CA_CRT" "$dir/tls.crt" >/dev/null || die "verification of $dir/tls.crt failed"
}

if [ -n "$CONTROLLER" ]; then
  # The control-plane chart's controller Service: <prefix> cut to 40, one
  # trailing '-' removed, then "-controller".
  api=${CONTROLLER:0:40}; api=${api%-}-controller
  i=0
  for suffix in "" ".$NAMESPACE" ".$NAMESPACE.svc" ".$NAMESPACE.svc.$CLUSTER_DOMAIN"; do
    i=$((i + 1)); echo "DNS.$i = $api$suffix" >>"$WORK/ext.cnf"
  done
  # Server auth only: the daemons take clients whose certificates are for
  # client auth, so this one can never drive them.
  issue "$OUT/controller-tls" "$api.$NAMESPACE.svc" server
  echo "issued the controller API's certificate $OUT/controller-tls/tls.crt (valid $DAYS days)"
  echo
  echo "Controller API Secret (set controller.api.tls.existingSecret=$api-tls in the control plane's values):"
  echo "  kubectl create secret generic $api-tls -n $NAMESPACE \\"
  echo "    --from-file=$OUT/controller-tls/tls.crt --from-file=$OUT/controller-tls/tls.key --from-file=$OUT/controller-tls/ca.crt"
  exit 0
fi

if [ "$SERVER" = 1 ]; then
  # The chart's headless Service name: <name> cut to 54 characters, one
  # trailing '-' removed, then "-headless".
  headless=${NAME:0:54}; headless=${headless%-}-headless
  i=0
  for host in "$NAME" "$headless"; do
    for suffix in "" ".$NAMESPACE" ".$NAMESPACE.svc" ".$NAMESPACE.svc.$CLUSTER_DOMAIN"; do
      i=$((i + 1)); echo "DNS.$i = $host$suffix" >>"$WORK/ext.cnf"
    done
  done
  p=0
  for san in $EXTRA_SANS; do
    case "$san" in
      DNS:*) i=$((i + 1)); echo "DNS.$i = ${san#DNS:}" >>"$WORK/ext.cnf" ;;
      IP:*) p=$((p + 1)); echo "IP.$p = ${san#IP:}" >>"$WORK/ext.cnf" ;;
    esac
  done
  issue "$OUT/server" "$NAME.$NAMESPACE.svc" server
  echo "issued server certificate $OUT/server/tls.crt (valid $DAYS days)"
fi
issue "$OUT/$CLIENT" "$CLIENT" client
echo "issued client certificate $OUT/$CLIENT/tls.crt (valid $DAYS days)"

echo
if [ "$SERVER" = 1 ]; then
  echo "Daemon Secret (set tls.existingSecret=$NAME-tls):"
  echo "  kubectl create secret generic $NAME-tls -n $NAMESPACE \\"
  echo "    --from-file=$OUT/server/tls.crt --from-file=$OUT/server/tls.key --from-file=$OUT/server/ca.crt"
fi
echo "Client Secret, in the namespace of the CI job pods:"
echo "  kubectl create secret generic $NAME-$CLIENT -n <jobs-namespace> \\"
echo "    --from-file=$OUT/$CLIENT/tls.crt --from-file=$OUT/$CLIENT/tls.key --from-file=$OUT/$CLIENT/ca.crt"
echo "Try it: buildctl --addr tcp://<address>:1234 --tlsdir $OUT/$CLIENT debug workers"
