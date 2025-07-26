# ATChess Deployment for abrah.ms

This deployment is specifically configured for the abrah.ms server which uses Caddy as its web server.

## Quick Setup

### 1. Run the Setup Script on Your Server

```bash
ssh root@abrah.ms
cd /tmp
wget https://raw.githubusercontent.com/justinabrahms/atchess/main/deploy/scripts/setup-atchess-abrahms.sh
chmod +x setup-atchess-abrahms.sh
sudo ./setup-atchess-abrahms.sh
```

### 2. Configure GitHub Actions Secrets

In your GitHub repository settings, add these secrets:

- `DEPLOY_HOST`: `abrah.ms`
- `DEPLOY_USER`: `atchess-deploy`
- `DEPLOY_PORT`: `22`
- `DEPLOY_SSH_KEY`: Your private SSH key for the atchess-deploy user

### 3. Add Your SSH Key to the Server

```bash
# On your local machine, copy your public key
cat ~/.ssh/id_ed25519.pub

# On the server, add it to the deploy user
ssh root@abrah.ms
echo "YOUR_PUBLIC_KEY" >> /home/atchess-deploy/.ssh/authorized_keys
```

### 4. Configure AT Protocol Credentials

```bash
ssh root@abrah.ms
sudo nano /etc/atchess/protocol.env
```

Edit the following values:
```env
ATPROTO_PDS_URL=https://bsky.social
ATPROTO_HANDLE=your-handle.bsky.social
ATPROTO_PASSWORD=your-app-password
```

### 5. Deploy

Push to main branch or run the workflow manually:

```bash
git push origin main
```

Or deploy manually:
```bash
./deploy/scripts/deploy-abrahms.sh
```

## Server Architecture

```
/srv/atchess/
├── app/          # Application binaries and static files
├── logs/         # Application logs
├── data/         # Persistent data
└── config/       # Runtime configuration

/etc/atchess/     # Environment configuration
/etc/caddy/conf.d/atchess.caddyfile  # Caddy configuration
```

## Services

- **atchess-protocol** - AT Protocol backend (port 8080)
- **atchess-web** - Web UI server (port 8081)

Both services run as the `atchess` user with security hardening.

## Useful Commands

```bash
# Check service status
atchess-status

# View logs
atchess-logs

# Restart services
atchess-restart

# Manual service control
sudo systemctl start/stop/restart atchess-protocol
sudo systemctl start/stop/restart atchess-web
```

## URLs

- Main site: https://atchess.abrah.ms
- API health: https://atchess.abrah.ms/api/health

## Security

- Services run as unprivileged user `atchess`
- Deployment via `atchess-deploy` user with limited sudo access
- SystemD security hardening (no new privileges, restricted filesystems)
- Caddy handles SSL/TLS automatically via Let's Encrypt
- Logs rotated daily with 14-day retention

## Troubleshooting

### Check Caddy configuration
```bash
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl status caddy
```

### Check service logs
```bash
sudo journalctl -u atchess-protocol -n 100
sudo journalctl -u atchess-web -n 100
```

### Check application logs
```bash
tail -f /srv/atchess/logs/protocol.log
tail -f /srv/atchess/logs/web.log
```

### Reload Caddy after config changes
```bash
sudo systemctl reload caddy
```