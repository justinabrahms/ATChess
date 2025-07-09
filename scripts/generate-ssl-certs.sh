#!/bin/bash

# Generate self-signed SSL certificates for local PDS development

echo "🔐 Generating SSL certificates for local PDS..."

# Create certs directory
mkdir -p certs

# Generate private key
openssl genrsa -out certs/localhost.key 2048

# Generate certificate signing request
openssl req -new -key certs/localhost.key -out certs/localhost.csr -subj "/C=US/ST=Local/L=Local/O=ATChess Dev/CN=localhost"

# Generate self-signed certificate
openssl x509 -req -in certs/localhost.csr -signkey certs/localhost.key -out certs/localhost.crt -days 365 -extensions v3_req -extfile <(cat << EOF
[v3_req]
keyUsage = keyEncipherment, dataEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names

[alt_names]
DNS.1 = localhost
DNS.2 = 127.0.0.1
IP.1 = 127.0.0.1
EOF
)

# Clean up CSR file
rm certs/localhost.csr

# Set appropriate permissions
chmod 644 certs/localhost.crt
chmod 600 certs/localhost.key

echo "✅ SSL certificates generated:"
echo "   Certificate: certs/localhost.crt"
echo "   Private Key: certs/localhost.key"
echo ""
echo "⚠️  These are self-signed certificates for development only!"
echo "   Your browser will show security warnings - this is expected."
echo ""
echo "🔒 The certificates are valid for:"
echo "   - localhost"
echo "   - 127.0.0.1"
echo ""
echo "📅 Certificate expires: $(date -d '+365 days' '+%Y-%m-%d')"