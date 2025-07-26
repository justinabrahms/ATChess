#!/bin/bash
set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Configuration
DOMAIN="${DOMAIN:-atchess.example.com}"
EMAIL="${EMAIL:-admin@example.com}"
APP_USER="atchess"
APP_DIR="/opt/atchess"
CONFIG_DIR="/etc/atchess"
WWW_DIR="/var/www/atchess"

echo -e "${GREEN}ATChess Server Setup Script${NC}"
echo "============================="
echo ""

# Check if running as root
if [[ $EUID -ne 0 ]]; then
   echo -e "${RED}This script must be run as root${NC}" 
   exit 1
fi

# Update system
echo -e "${YELLOW}Updating system packages...${NC}"
apt-get update
apt-get upgrade -y

# Install dependencies
echo -e "${YELLOW}Installing dependencies...${NC}"
apt-get install -y \
    nginx \
    certbot \
    python3-certbot-nginx \
    git \
    curl \
    ufw \
    fail2ban \
    htop \
    tmux

# Create application user
echo -e "${YELLOW}Creating application user...${NC}"
if ! id "$APP_USER" &>/dev/null; then
    useradd -r -s /bin/false -m -d /home/$APP_USER $APP_USER
    echo -e "${GREEN}User $APP_USER created${NC}"
else
    echo -e "${GREEN}User $APP_USER already exists${NC}"
fi

# Create directories
echo -e "${YELLOW}Creating directories...${NC}"
mkdir -p $APP_DIR/{bin,data,logs}
mkdir -p $CONFIG_DIR
mkdir -p $WWW_DIR
mkdir -p /var/log/atchess

# Set permissions
chown -R $APP_USER:$APP_USER $APP_DIR
chown -R $APP_USER:$APP_USER $CONFIG_DIR
chown -R $APP_USER:$APP_USER /var/log/atchess
chown -R www-data:www-data $WWW_DIR

# Configure firewall
echo -e "${YELLOW}Configuring firewall...${NC}"
ufw default deny incoming
ufw default allow outgoing
ufw allow ssh
ufw allow 80/tcp
ufw allow 443/tcp
ufw --force enable

# Configure fail2ban
echo -e "${YELLOW}Configuring fail2ban...${NC}"
cat > /etc/fail2ban/jail.local <<EOF
[DEFAULT]
bantime = 3600
findtime = 600
maxretry = 5

[sshd]
enabled = true

[nginx-http-auth]
enabled = true

[nginx-limit-req]
enabled = true
EOF

systemctl restart fail2ban

# Create environment files
echo -e "${YELLOW}Creating environment configuration files...${NC}"
cat > $CONFIG_DIR/protocol.env <<EOF
# ATChess Protocol Service Configuration
# Edit these values for your deployment

# AT Protocol Configuration
ATCHESS_ATPROTO_PDS_URL=https://bsky.social
ATCHESS_ATPROTO_HANDLE=your-handle.bsky.social
ATCHESS_ATPROTO_PASSWORD=your-app-password

# Server Configuration
ATCHESS_SERVER_HOST=0.0.0.0
ATCHESS_SERVER_PORT=8080

# Development Configuration
ATCHESS_DEVELOPMENT_DEBUG=false
ATCHESS_DEVELOPMENT_LOG_LEVEL=info

# Firehose Configuration
ATCHESS_FIREHOSE_ENABLED=false
ATCHESS_FIREHOSE_URL=wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos
EOF

cat > $CONFIG_DIR/web.env <<EOF
# ATChess Web Service Configuration
# Edit these values for your deployment

# Server Configuration
ATCHESS_SERVER_HOST=0.0.0.0
ATCHESS_SERVER_PORT=8081

# Protocol Service URL (internal)
ATCHESS_PROTOCOL_URL=http://localhost:8080

# Development Configuration
ATCHESS_DEVELOPMENT_DEBUG=false
ATCHESS_DEVELOPMENT_LOG_LEVEL=info
EOF

# Set permissions on config files
chmod 600 $CONFIG_DIR/*.env
chown $APP_USER:$APP_USER $CONFIG_DIR/*.env

# Update nginx configuration with actual domain
echo -e "${YELLOW}Updating Nginx configuration...${NC}"
sed -i "s/atchess.example.com/$DOMAIN/g" /etc/nginx/sites-available/atchess.conf

# Obtain SSL certificate
echo -e "${YELLOW}Obtaining SSL certificate...${NC}"
echo -e "${YELLOW}Make sure your domain $DOMAIN points to this server!${NC}"
read -p "Press enter to continue with SSL certificate generation..."

certbot --nginx -d $DOMAIN --non-interactive --agree-tos --email $EMAIL --redirect

# Create systemd log configuration
echo -e "${YELLOW}Configuring logging...${NC}"
cat > /etc/rsyslog.d/30-atchess.conf <<EOF
if \$programname == 'atchess-protocol' then /var/log/atchess/protocol.log
if \$programname == 'atchess-web' then /var/log/atchess/web.log
& stop
EOF

# Create logrotate configuration
cat > /etc/logrotate.d/atchess <<EOF
/var/log/atchess/*.log {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    notifempty
    create 0640 $APP_USER $APP_USER
    sharedscripts
    postrotate
        systemctl reload rsyslog > /dev/null 2>&1 || true
    endscript
}
EOF

systemctl restart rsyslog

# Create helper scripts
echo -e "${YELLOW}Creating helper scripts...${NC}"
cat > /usr/local/bin/atchess-logs <<'EOF'
#!/bin/bash
case "$1" in
    protocol)
        journalctl -u atchess-protocol -f
        ;;
    web)
        journalctl -u atchess-web -f
        ;;
    nginx)
        tail -f /var/log/nginx/atchess_*.log
        ;;
    all)
        tmux new-session -d -s logs
        tmux split-window -h
        tmux split-window -v
        tmux select-pane -t 0
        tmux split-window -v
        
        tmux send-keys -t 0 'journalctl -u atchess-protocol -f' C-m
        tmux send-keys -t 1 'journalctl -u atchess-web -f' C-m
        tmux send-keys -t 2 'tail -f /var/log/nginx/atchess_access.log' C-m
        tmux send-keys -t 3 'tail -f /var/log/nginx/atchess_error.log' C-m
        
        tmux attach-session -t logs
        ;;
    *)
        echo "Usage: atchess-logs {protocol|web|nginx|all}"
        exit 1
        ;;
esac
EOF

chmod +x /usr/local/bin/atchess-logs

# Create status script
cat > /usr/local/bin/atchess-status <<'EOF'
#!/bin/bash
echo "ATChess Service Status"
echo "====================="
echo ""
echo "Services:"
systemctl status atchess-protocol --no-pager | grep "Active:"
systemctl status atchess-web --no-pager | grep "Active:"
systemctl status nginx --no-pager | grep "Active:"
echo ""
echo "Ports:"
ss -tlnp | grep -E "(8080|8081|80|443)" || echo "No services listening"
echo ""
echo "Disk usage:"
df -h $APP_DIR
echo ""
echo "Memory usage:"
free -h
EOF

chmod +x /usr/local/bin/atchess-status

# Final instructions
echo ""
echo -e "${GREEN}Setup complete!${NC}"
echo ""
echo "Next steps:"
echo "1. Edit the configuration files in $CONFIG_DIR/"
echo "2. Deploy your application binaries to $APP_DIR/bin/"
echo "3. Start the services:"
echo "   systemctl start atchess-protocol"
echo "   systemctl start atchess-web"
echo ""
echo "Useful commands:"
echo "- atchess-status   : Check service status"
echo "- atchess-logs all : View all logs in tmux"
echo "- systemctl restart atchess-protocol"
echo "- systemctl restart atchess-web"
echo ""
echo -e "${YELLOW}Don't forget to update your GitHub secrets:${NC}"
echo "- DEPLOY_HOST: $DOMAIN or server IP"
echo "- DEPLOY_USER: $USER"
echo "- DEPLOY_PORT: 22"
echo "- DEPLOY_SSH_KEY: Your private SSH key"