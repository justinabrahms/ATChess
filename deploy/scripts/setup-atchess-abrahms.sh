#!/bin/bash

# ATChess Server Deployment Script for atchess.abrah.ms
# This script handles server deployment including OAuth key management

set -euo pipefail

# Configuration for atchess.abrah.ms deployment
# Match the paths used by the auto-deploy workflow
ATCHESS_DIR="/srv/atchess/app"
ATCHESS_USER="atchess"
KEY_DIR="/etc/atchess/keys"
OAUTH_KEY_PATH="$KEY_DIR/oauth-private-key.pem"
SERVICE_NAME="atchess-protocol"
DOMAIN="atchess.abrah.ms"

echo "🚀 ATChess Server Deployment for $DOMAIN"
echo "========================================"

# Check if running as root/sudo
if [[ $EUID -ne 0 ]]; then 
    echo "❌ This script must be run as root"
    exit 1
fi

# Create service user if doesn't exist
if ! id -u "$ATCHESS_USER" >/dev/null 2>&1; then
    echo "👤 Creating service user..."
    useradd -r -s /bin/false -d /var/lib/atchess -m "$ATCHESS_USER"
fi

# Create directory structure
echo "📁 Setting up directories..."
mkdir -p "$ATCHESS_DIR"
mkdir -p "/etc/atchess"
mkdir -p "$KEY_DIR"
mkdir -p "/var/log/atchess"

# Set proper permissions on key directory
chown root:"$ATCHESS_USER" "$KEY_DIR"
chmod 750 "$KEY_DIR"

# Note: This setup script assumes your auto-deploy workflow handles binary deployment
# We only need to ensure directories exist and download the key generator if needed

# Create required directories
echo "📁 Setting up directory structure..."
mkdir -p "$ATCHESS_DIR/bin"
mkdir -p "$ATCHESS_DIR/web/static"
mkdir -p "$ATCHESS_DIR/lexicons"

# Download just the OAuth key generator if it doesn't exist
if [ ! -f "$ATCHESS_DIR/bin/generate-oauth-keys" ]; then
    echo "📥 Downloading OAuth key generator..."
    TEMP_DIR=$(mktemp -d)
    cd "$TEMP_DIR"
    
    # Download the key generator from the latest build
    if ! wget -q "https://github.com/justinabrahms/atchess/releases/latest/download/generate-oauth-keys" -O generate-oauth-keys; then
        echo "⚠️  Could not download key generator, will build it locally..."
        # Fallback: download just the source for the key generator
        wget -q "https://raw.githubusercontent.com/justinabrahms/atchess/main/cmd/generate-oauth-keys/main.go" -O main.go
        if command -v go &> /dev/null; then
            go build -o generate-oauth-keys main.go
        else
            echo "❌ Go is not installed and key generator download failed"
            echo "   Please install Go or manually create OAuth keys"
            rm -rf "$TEMP_DIR"
            exit 1
        fi
    fi
    
    # Install the key generator
    cp generate-oauth-keys "$ATCHESS_DIR/bin/"
    chmod +x "$ATCHESS_DIR/bin/generate-oauth-keys"
    cd /
    rm -rf "$TEMP_DIR"
fi

# Ensure ownership is correct
chown -R "$ATCHESS_USER:$ATCHESS_USER" "$ATCHESS_DIR"

echo "✅ Directory structure ready for auto-deployment"

# Handle OAuth key
echo "🔐 Setting up OAuth authentication..."
if [ ! -f "$OAUTH_KEY_PATH" ]; then
    echo "⚠️  No OAuth private key found. Generating new key pair..."
    
    # Use the pre-built key generator
    cd "$ATCHESS_DIR"
    
    # Generate key with restricted umask for security
    TEMP_KEY=$(mktemp)
    sudo -u "$ATCHESS_USER" "$ATCHESS_DIR/bin/generate-oauth-keys" > "$TEMP_KEY"
    
    # Extract private key with proper permissions from the start
    (umask 077 && sed -n '/-----BEGIN EC PRIVATE KEY-----/,/-----END EC PRIVATE KEY-----/p' "$TEMP_KEY" > "$OAUTH_KEY_PATH")
    
    # Set ownership and read-only permissions
    chown "$ATCHESS_USER:$ATCHESS_USER" "$OAUTH_KEY_PATH"
    chmod 400 "$OAUTH_KEY_PATH"
    
    # Clean up
    rm -f "$TEMP_KEY"
    
    echo "✅ New OAuth key generated and saved with secure permissions"
else
    echo "✅ OAuth key already exists, preserving it"
    # Ensure permissions are correct even for existing keys
    chown "$ATCHESS_USER:$ATCHESS_USER" "$OAUTH_KEY_PATH"
    chmod 400 "$OAUTH_KEY_PATH"
fi

# Create environment files (matching existing deployment structure)
echo "📝 Creating environment configuration..."

# Create protocol.env for protocol service
PROTOCOL_ENV="/etc/atchess/protocol.env"
if [ -f "$PROTOCOL_ENV" ]; then
    echo "📋 Preserving existing protocol environment configuration..."
    # Load existing values
    source "$PROTOCOL_ENV" 2>/dev/null || true
fi

# Only create/update if it doesn't exist or is missing required vars
if [ ! -f "$PROTOCOL_ENV" ] || ! grep -q "SERVER_BASE_URL" "$PROTOCOL_ENV" 2>/dev/null; then
    cat > "$PROTOCOL_ENV" <<EOF
# ATChess Protocol Service Environment Configuration
OAUTH_PRIVATE_KEY_PATH=$OAUTH_KEY_PATH
ATPROTO_PDS_URL=${ATPROTO_PDS_URL:-https://bsky.social}
ATPROTO_HANDLE=${ATPROTO_HANDLE:-}
ATPROTO_PASSWORD=${ATPROTO_PASSWORD:-}
ATPROTO_USE_DPOP=true
SERVER_BASE_URL=https://$DOMAIN
SERVER_HOST=0.0.0.0
SERVER_PORT=8080
EOF
    chmod 644 "$PROTOCOL_ENV"
    echo "✅ Created/updated protocol.env"
else
    echo "✅ protocol.env already exists with required configuration"
fi

# Create web.env for web service (if needed)
WEB_ENV="/etc/atchess/web.env"
if [ ! -f "$WEB_ENV" ]; then
    cat > "$WEB_ENV" <<EOF
# ATChess Web Service Environment Configuration
SERVER_HOST=0.0.0.0
SERVER_PORT=8081
EOF
    chmod 644 "$WEB_ENV"
fi

# Create systemd service file
echo "🔧 Creating systemd service..."
cat > /etc/systemd/system/$SERVICE_NAME.service <<EOF
[Unit]
Description=ATChess Protocol Service
After=network.target

[Service]
Type=simple
User=$ATCHESS_USER
Group=$ATCHESS_USER
WorkingDirectory=$ATCHESS_DIR
ExecStart=$ATCHESS_DIR/bin/atchess-protocol
Environment="ATCHESS_STATIC_DIR=$ATCHESS_DIR/web/static"
Environment="ATCHESS_LEXICONS_DIR=$ATCHESS_DIR/lexicons"
Restart=always
RestartSec=10
StandardOutput=append:/var/log/atchess/protocol.log
StandardError=append:/var/log/atchess/protocol-error.log
EnvironmentFile=/etc/atchess/protocol.env

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/atchess
ReadOnlyPaths=/etc/atchess/keys

[Install]
WantedBy=multi-user.target
EOF

# Create web service file
cat > /etc/systemd/system/atchess-web.service <<EOF
[Unit]
Description=ATChess Web Service
After=network.target

[Service]
Type=simple
User=$ATCHESS_USER
Group=$ATCHESS_USER
WorkingDirectory=$ATCHESS_DIR
ExecStart=$ATCHESS_DIR/bin/atchess-web
Environment="ATCHESS_STATIC_DIR=$ATCHESS_DIR/web/static"
Restart=always
RestartSec=10
StandardOutput=append:/var/log/atchess/web.log
StandardError=append:/var/log/atchess/web-error.log
EnvironmentFile=/etc/atchess/web.env

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/atchess
ReadOnlyPaths=/etc/atchess/keys

[Install]
WantedBy=multi-user.target
EOF

# Set up log rotation
echo "📋 Setting up log rotation..."
cat > /etc/logrotate.d/atchess <<EOF
/var/log/atchess/*.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    create 0640 $ATCHESS_USER $ATCHESS_USER
    sharedscripts
    postrotate
        systemctl reload $SERVICE_NAME >/dev/null 2>&1 || true
        systemctl reload atchess-web >/dev/null 2>&1 || true
    endscript
}
EOF

# Set permissions
chown -R "$ATCHESS_USER:$ATCHESS_USER" /var/log/atchess

# Reload systemd
echo "🔄 Reloading systemd configuration..."
systemctl daemon-reload

# Enable services if not already enabled
systemctl enable $SERVICE_NAME atchess-web 2>/dev/null || true

# Restart services
echo "🚀 Restarting services..."
systemctl restart $SERVICE_NAME atchess-web

# Wait for services to start
sleep 3

# Check service status
if systemctl is-active --quiet $SERVICE_NAME; then
    echo "✅ Protocol service is running"
else
    echo "❌ Protocol service failed to start"
    journalctl -u $SERVICE_NAME -n 50
    exit 1
fi

if systemctl is-active --quiet atchess-web; then
    echo "✅ Web service is running"
else
    echo "❌ Web service failed to start"
    journalctl -u atchess-web -n 50
    exit 1
fi

# Test endpoints
echo "🧪 Testing endpoints..."
if curl -f http://localhost:8080/api/health &> /dev/null; then
    echo "✅ Protocol API is responding"
else
    echo "❌ Protocol API is not responding"
fi

if curl -f http://localhost:8081 &> /dev/null; then
    echo "✅ Web interface is responding"
else
    echo "❌ Web interface is not responding"
fi

if curl -f http://localhost:8080/client-metadata.json &> /dev/null; then
    echo "✅ OAuth client metadata is accessible"
else
    echo "❌ OAuth client metadata is not accessible"
fi

echo ""
echo "🎉 Deployment complete!"
echo "====================="
echo ""
echo "📍 Services running:"
echo "   - Protocol API: http://localhost:8080"
echo "   - Web Interface: http://localhost:8081"
echo "   - OAuth Metadata: http://localhost:8080/client-metadata.json"
echo ""
echo "🔐 OAuth private key: $OAUTH_KEY_PATH"
echo "📝 Environment config: /etc/atchess/environment"
echo "📋 Logs: /var/log/atchess/"
echo ""
echo "💡 Management commands:"
echo "   - View logs: journalctl -u $SERVICE_NAME -f"
echo "   - Restart: systemctl restart $SERVICE_NAME"
echo "   - Stop: systemctl stop $SERVICE_NAME atchess-web"
echo ""

# Check for missing required environment variables
MISSING_VARS=()
if [ -z "${ATPROTO_HANDLE:-}" ]; then
    MISSING_VARS+=("ATPROTO_HANDLE")
fi
if [ -z "${ATPROTO_PASSWORD:-}" ]; then
    MISSING_VARS+=("ATPROTO_PASSWORD")
fi

if [ ${#MISSING_VARS[@]} -gt 0 ]; then
    echo "⚠️  IMPORTANT: The following environment variables need to be configured in /etc/atchess/environment:"
    for var in "${MISSING_VARS[@]}"; do
        echo "   - $var"
    done
    echo ""
    echo "   Edit the file: sudo nano /etc/atchess/environment"
    echo "   Then restart: sudo systemctl restart $SERVICE_NAME"
else
    echo "✅ All required environment variables are configured"
fi