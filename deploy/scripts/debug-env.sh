#!/bin/bash
# Debug environment loading for ATChess protocol service

echo "=== Environment Debug Script ==="
echo ""

# Check what the systemd service sees
echo "1. Checking systemd environment file:"
if [ -f /etc/atchess/protocol.env ]; then
    echo "File exists at /etc/atchess/protocol.env"
    echo "Contents (without password):"
    grep -v PASSWORD /etc/atchess/protocol.env | head -20
else
    echo "ERROR: /etc/atchess/protocol.env not found!"
fi

echo ""
echo "2. Testing environment loading:"
cd /srv/atchess/app

# Source the environment file like systemd would
set -a
source /etc/atchess/protocol.env
set +a

echo "Environment variables set:"
env | grep -E "(ATPROTO|ATCHESS|SERVER)" | grep -v PASSWORD | sort

echo ""
echo "3. Checking for config.yaml:"
if [ -f config.yaml ]; then
    echo "Found config.yaml"
    cat config.yaml
elif [ -f ./config/config.yaml ]; then
    echo "Found ./config/config.yaml"
    cat ./config/config.yaml
else
    echo "No config.yaml found - app will use defaults!"
    echo "This is likely why it's using localhost:3000"
fi

echo ""
echo "4. Creating minimal config.yaml from environment:"
cat > /tmp/config.yaml << EOF
server:
  host: "${SERVER_HOST:-0.0.0.0}"
  port: ${SERVER_PORT:-8080}

atproto:
  pds_url: "${ATPROTO_PDS_URL:-https://bsky.social}"
  handle: "${ATPROTO_HANDLE}"
  password: "${ATPROTO_PASSWORD}"
  use_dpop: ${ATPROTO_USE_DPOP:-false}

development:
  debug: ${DEVELOPMENT_DEBUG:-true}
  log_level: "${DEVELOPMENT_LOG_LEVEL:-info}"

firehose:
  enabled: ${FIREHOSE_ENABLED:-false}
  url: "${FIREHOSE_URL:-wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos}"
EOF

echo "Created config.yaml at /tmp/config.yaml"
echo "To use it: sudo cp /tmp/config.yaml /srv/atchess/app/config.yaml"