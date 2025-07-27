#!/bin/bash
# ATChess Setup Script for abrah.ms server (with existing Caddy)
# This script sets up ATChess on the existing server infrastructure

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Configuration
DOMAIN="${DOMAIN:-atchess.abrah.ms}"
APP_USER="atchess"
DEPLOY_USER="atchess-deploy"
APP_DIR="/srv/atchess"
CONFIG_DIR="/etc/atchess"

echo -e "${GREEN}ATChess Setup for abrah.ms Server${NC}"
echo "===================================="
echo ""
echo "Domain: $DOMAIN"
echo ""

# Check if running as root
if [[ $EUID -ne 0 ]]; then
   echo -e "${RED}This script must be run as root${NC}" 
   exit 1
fi

# Create application user (no shell, no home)
echo -e "${YELLOW}Creating application user...${NC}"
if ! id "$APP_USER" &>/dev/null; then
    useradd -r -s /bin/false -d /nonexistent -c "ATChess Service" $APP_USER
    echo -e "${GREEN}User $APP_USER created${NC}"
else
    echo -e "${GREEN}User $APP_USER already exists${NC}"
fi

# Create deployment user
echo -e "${YELLOW}Creating deployment user...${NC}"
if ! id "$DEPLOY_USER" &>/dev/null; then
    useradd -m -s /bin/bash $DEPLOY_USER
    usermod -a -G www $DEPLOY_USER
    echo -e "${GREEN}User $DEPLOY_USER created${NC}"
else
    echo -e "${GREEN}User $DEPLOY_USER already exists${NC}"
fi

# Create directory structure
echo -e "${YELLOW}Creating application directories...${NC}"
mkdir -p $APP_DIR/{app,logs,data,config}
mkdir -p $CONFIG_DIR
mkdir -p /var/log/caddy/$DOMAIN

# Set permissions following server conventions
chown -R $APP_USER:www $APP_DIR
chmod 750 $APP_DIR
chmod 770 $APP_DIR/logs $APP_DIR/data
chown -R $APP_USER:www $CONFIG_DIR
chmod 750 $CONFIG_DIR

# Create Caddy configuration directory
echo -e "${YELLOW}Setting up Caddy configuration...${NC}"
mkdir -p /etc/caddy/conf.d

# Check if Caddyfile has import directive
if ! grep -q "import /etc/caddy/conf.d/\*.caddyfile" /etc/caddy/Caddyfile 2>/dev/null; then
    echo "" >> /etc/caddy/Caddyfile
    echo "# Import additional configurations" >> /etc/caddy/Caddyfile
    echo "import /etc/caddy/conf.d/*.caddyfile" >> /etc/caddy/Caddyfile
fi

# Create ATChess Caddy configuration
cat > /etc/caddy/conf.d/atchess.caddyfile << EOF
# ATChess Configuration
$DOMAIN {
    tls {
        dns lego_deprecated dnsimple
    }
    
    encode gzip
    
    # API endpoints - reverse proxy to protocol service
    handle /api/* {
        reverse_proxy localhost:8080 {
            health_uri /api/health
            health_interval 30s
        }
    }
    
    # WebSocket endpoint for real-time updates
    handle /api/ws {
        reverse_proxy localhost:8080 {
            header_up Upgrade {http.upgrade}
            header_up Connection "upgrade"
        }
    }
    
    # Static files and web UI
    handle {
        reverse_proxy localhost:8081
    }
    
    # Security headers
    header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains"
        X-Content-Type-Options "nosniff"
        X-Frame-Options "DENY"
        Referrer-Policy "strict-origin-when-cross-origin"
    }
    
    # Logging
    log {
        output file /var/log/caddy/$DOMAIN/access.log {
            roll_keep 3
        }
    }
}
EOF

# Create systemd service files
echo -e "${YELLOW}Creating systemd service files...${NC}"

# Protocol service
cat > /etc/systemd/system/atchess-protocol.service << 'EOF'
[Unit]
Description=ATChess Protocol Service
After=network.target

[Service]
Type=simple
User=atchess
Group=www
WorkingDirectory=/srv/atchess/app
EnvironmentFile=/etc/atchess/protocol.env

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/srv/atchess/data /srv/atchess/logs
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
RestrictRealtime=true
RestrictSUIDSGID=true
RemoveIPC=true
PrivateMounts=true

# Resource limits
LimitNOFILE=1024
LimitNPROC=512

ExecStart=/srv/atchess/app/atchess-protocol
Restart=on-failure
RestartSec=5

StandardOutput=append:/srv/atchess/logs/protocol.log
StandardError=append:/srv/atchess/logs/protocol-error.log

[Install]
WantedBy=multi-user.target
EOF

# Web service
cat > /etc/systemd/system/atchess-web.service << 'EOF'
[Unit]
Description=ATChess Web Service
After=network.target atchess-protocol.service

[Service]
Type=simple
User=atchess
Group=www
WorkingDirectory=/srv/atchess/app
EnvironmentFile=/etc/atchess/web.env

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/srv/atchess/data /srv/atchess/logs
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
RestrictRealtime=true
RestrictSUIDSGID=true
RemoveIPC=true
PrivateMounts=true

# Resource limits
LimitNOFILE=1024
LimitNPROC=512

ExecStart=/srv/atchess/app/atchess-web
Restart=on-failure
RestartSec=5

StandardOutput=append:/srv/atchess/logs/web.log
StandardError=append:/srv/atchess/logs/web-error.log

[Install]
WantedBy=multi-user.target
EOF

# Create environment file templates
echo -e "${YELLOW}Creating environment configuration templates...${NC}"

cat > $CONFIG_DIR/protocol.env << 'EOF'
# ATChess Protocol Service Configuration
# 
# The service will start in demo mode with these defaults.
# To connect to a real AT Protocol network:
# 1. Replace ATPROTO_HANDLE with your Bluesky handle
# 2. Create an app password at https://bsky.app/settings/app-passwords
# 3. Replace ATPROTO_PASSWORD with your app password
# 4. Restart the service: sudo systemctl restart atchess-protocol

# Server
SERVER_HOST=0.0.0.0
SERVER_PORT=8080

# AT Protocol - REQUIRED for service to start
# NOTE: The protocol service REQUIRES valid AT Protocol credentials to run.
# Without valid credentials, the service will fail to start.
# 
# To get started:
# 1. Create a Bluesky account at https://bsky.app
# 2. Go to Settings > App Passwords
# 3. Create a new app password for ATChess
# 4. Update the values below with your credentials
#
# Default values (service will NOT start with these):
ATPROTO_PDS_URL=https://bsky.social
ATPROTO_HANDLE=your-handle.bsky.social
ATPROTO_PASSWORD=your-app-password-here
ATPROTO_USE_DPOP=false

# Firehose (optional)
FIREHOSE_ENABLED=false
FIREHOSE_URL=wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos

# Development
DEVELOPMENT_DEBUG=true
DEVELOPMENT_LOG_LEVEL=info

# Alternative environment variable names (both work)
# These mirror the above settings with ATCHESS_ prefix
ATCHESS_SERVER_HOST=0.0.0.0
ATCHESS_SERVER_PORT=8080
ATCHESS_ATPROTO_PDS_URL=https://bsky.social
ATCHESS_ATPROTO_HANDLE=your-handle.bsky.social
ATCHESS_ATPROTO_PASSWORD=your-app-password-here
ATCHESS_DEVELOPMENT_DEBUG=true
ATCHESS_DEVELOPMENT_LOG_LEVEL=info
EOF

cat > $CONFIG_DIR/web.env << 'EOF'
# ATChess Web Service Configuration

# Server
SERVER_HOST=0.0.0.0
SERVER_PORT=8081

# API
API_URL=http://localhost:8080/api

# Development
DEVELOPMENT_DEBUG=false
DEVELOPMENT_LOG_LEVEL=info
EOF

# Set permissions on config files
chown $APP_USER:www $CONFIG_DIR/*.env
chmod 640 $CONFIG_DIR/*.env

# Create sudo rules for deployment
echo -e "${YELLOW}Setting up deployment permissions...${NC}"
cat > /etc/sudoers.d/atchess-deploy << 'EOF'
# Allow atchess-deploy to manage atchess services
atchess-deploy ALL=(root) NOPASSWD: /bin/systemctl restart atchess-protocol
atchess-deploy ALL=(root) NOPASSWD: /bin/systemctl restart atchess-web
atchess-deploy ALL=(root) NOPASSWD: /bin/systemctl stop atchess-protocol
atchess-deploy ALL=(root) NOPASSWD: /bin/systemctl stop atchess-web
atchess-deploy ALL=(root) NOPASSWD: /bin/systemctl start atchess-protocol
atchess-deploy ALL=(root) NOPASSWD: /bin/systemctl start atchess-web
atchess-deploy ALL=(root) NOPASSWD: /bin/systemctl status atchess-protocol
atchess-deploy ALL=(root) NOPASSWD: /bin/systemctl status atchess-web
atchess-deploy ALL=(root) NOPASSWD: /bin/systemctl daemon-reload
EOF

# Create deployment directory permissions
echo -e "${YELLOW}Setting up deployment access...${NC}"
mkdir -p /home/$DEPLOY_USER/.ssh
touch /home/$DEPLOY_USER/.ssh/authorized_keys
chown -R $DEPLOY_USER:$DEPLOY_USER /home/$DEPLOY_USER/.ssh
chmod 700 /home/$DEPLOY_USER/.ssh
chmod 600 /home/$DEPLOY_USER/.ssh/authorized_keys

# Give deploy user write access to app directory
usermod -a -G www $DEPLOY_USER
chmod -R g+w $APP_DIR/app

# Create logrotate configuration
echo -e "${YELLOW}Setting up log rotation...${NC}"
cat > /etc/logrotate.d/atchess << 'EOF'
/srv/atchess/logs/*.log {
    daily
    missingok
    rotate 14
    compress
    delaycompress
    notifempty
    create 0640 atchess www
    sharedscripts
    postrotate
        systemctl reload atchess-protocol > /dev/null 2>&1 || true
        systemctl reload atchess-web > /dev/null 2>&1 || true
    endscript
}
EOF

# Create helper scripts
echo -e "${YELLOW}Creating helper commands...${NC}"
cat > /usr/local/bin/atchess-logs << 'EOF'
#!/bin/bash
# Show logs for ATChess services
journalctl -u atchess-protocol -u atchess-web -f
EOF
chmod +x /usr/local/bin/atchess-logs

cat > /usr/local/bin/atchess-status << 'EOF'
#!/bin/bash
# Show status of ATChess services
systemctl status atchess-protocol atchess-web
EOF
chmod +x /usr/local/bin/atchess-status

cat > /usr/local/bin/atchess-restart << 'EOF'
#!/bin/bash
# Restart ATChess services
systemctl restart atchess-protocol atchess-web
EOF
chmod +x /usr/local/bin/atchess-restart

# Reload systemd
systemctl daemon-reload

# Reload Caddy to pick up new configuration
echo -e "${YELLOW}Reloading Caddy...${NC}"
systemctl reload caddy

echo -e "${GREEN}Setup complete!${NC}"
echo ""
echo -e "${RED}IMPORTANT: The protocol service requires AT Protocol credentials!${NC}"
echo ""
echo -e "${YELLOW}Required steps before starting:${NC}"
echo "1. Create a Bluesky account at: https://bsky.app"
echo "2. Create an app password at: https://bsky.app/settings/app-passwords" 
echo "3. Edit $CONFIG_DIR/protocol.env and update:"
echo "   - ATPROTO_HANDLE (your Bluesky handle)"
echo "   - ATPROTO_PASSWORD (your app password)"
echo ""
echo -e "${YELLOW}Next steps:${NC}"
echo "1. Add your SSH public key to: /home/$DEPLOY_USER/.ssh/authorized_keys"
echo "2. Configure AT Protocol credentials (see above)"
echo "3. Deploy the ATChess binaries to: $APP_DIR/app/"
echo "4. Start services: systemctl start atchess-protocol atchess-web"
echo ""
echo -e "${YELLOW}Note:${NC} The web service will run without AT Protocol credentials,"
echo "but the protocol service will fail to start until valid credentials are provided."
echo ""
echo -e "${YELLOW}Useful commands:${NC}"
echo "  atchess-status  - Check service status"
echo "  atchess-logs    - View service logs"
echo "  atchess-restart - Restart services"
echo ""
echo -e "${YELLOW}GitHub Actions deployment:${NC}"
echo "  DEPLOY_USER: $DEPLOY_USER"
echo "  DEPLOY_HOST: $(hostname -f)"
echo "  Application will be available at: https://$DOMAIN"