#!/bin/bash
# Wrapper script to debug and run the protocol service with environment variables

# Source the environment file if it exists
if [ -f /etc/atchess/protocol.env ]; then
    echo "Loading environment from /etc/atchess/protocol.env"
    # Export variables from the environment file
    set -a
    source /etc/atchess/protocol.env
    set +a
else
    echo "Warning: /etc/atchess/protocol.env not found"
fi

# Debug: Print environment variables
echo "Environment variables:"
env | grep -E "(SERVER_|ATPROTO_|FIREHOSE_|DEVELOPMENT_|ATCHESS_)" | sort

# Change to app directory
cd /srv/atchess/app || exit 1

# Run the protocol service
exec /srv/atchess/app/atchess-protocol