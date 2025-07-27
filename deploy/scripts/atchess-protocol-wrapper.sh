#!/bin/bash
# Wrapper script for atchess-protocol to ensure environment variables are loaded

# Source the environment file if it exists
if [ -f /etc/atchess/protocol.env ]; then
    set -a  # Mark all new variables for export
    source /etc/atchess/protocol.env
    set +a
fi

# Execute the actual binary
exec /srv/atchess/app/atchess-protocol "$@"