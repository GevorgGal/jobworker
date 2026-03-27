#!/usr/bin/env bash
#
# Generates a self-signed CA and issues server + client certificates for
# the job worker service prototype.
#
# Crypto choices:
#   - ECDSA P-256 keys (fast, widely supported, strong)
#   - SHA-256 signatures
#   - TLS 1.3 enforced at the Go layer (tls.Config), not at cert level
#
# Certificate hierarchy:
#   CA (self-signed, 10y) ─┬─ server (localhost SAN, 1y)
#                          ├─ admin  (CN=admin,  client auth, 1y)
#                          └─ viewer (CN=viewer, client auth, 1y)
#
# TODO: Never commit private keys in production. Use a secrets manager
# or automate cert issuance via a proper CA (e.g., Vault, step-ca).

set -euo pipefail

cd "$(dirname "$0")"
rm -f *.pem

CA_DAYS=3650
LEAF_DAYS=365

# --- CA ---
openssl ecparam -name prime256v1 -genkey -noout -out ca-key.pem

openssl req -new -x509 -key ca-key.pem -out ca.pem -days "$CA_DAYS" \
    -subj "/CN=JobWorker CA" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
    -addext "keyUsage=critical,keyCertSign,cRLSign"

# --- Server ---
openssl ecparam -name prime256v1 -genkey -noout -out server-key.pem

openssl req -new -key server-key.pem -out server.csr \
    -subj "/CN=jobworker-server"

openssl x509 -req -in server.csr -CA ca.pem -CAkey ca-key.pem \
    -CAcreateserial -out server.pem -days "$LEAF_DAYS" \
    -extfile <(printf "subjectAltName=DNS:localhost,IP:127.0.0.1\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth")

# --- Client: admin (CN=admin) ---
openssl ecparam -name prime256v1 -genkey -noout -out admin-key.pem

openssl req -new -key admin-key.pem -out admin.csr \
    -subj "/CN=admin"

openssl x509 -req -in admin.csr -CA ca.pem -CAkey ca-key.pem \
    -CAcreateserial -out admin.pem -days "$LEAF_DAYS" \
    -extfile <(printf "keyUsage=critical,digitalSignature\nextendedKeyUsage=clientAuth")

# --- Client: viewer (CN=viewer) ---
openssl ecparam -name prime256v1 -genkey -noout -out viewer-key.pem

openssl req -new -key viewer-key.pem -out viewer.csr \
    -subj "/CN=viewer"

openssl x509 -req -in viewer.csr -CA ca.pem -CAkey ca-key.pem \
    -CAcreateserial -out viewer.pem -days "$LEAF_DAYS" \
    -extfile <(printf "keyUsage=critical,digitalSignature\nextendedKeyUsage=clientAuth")

# --- Cleanup ---
rm -f *.csr *.srl
chmod 600 *-key.pem

echo "Certificates generated successfully."