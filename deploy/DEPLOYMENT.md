# ATChess Deployment Guide

This guide covers deploying ATChess to a DigitalOcean droplet with automated GitHub Actions deployment.

## Prerequisites

1. **DigitalOcean Droplet**
   - Ubuntu 22.04 LTS or newer
   - Minimum 2GB RAM, 2 vCPUs
   - SSH access configured

2. **Domain Name**
   - Point your domain (e.g., `atchess.example.com`) to your droplet's IP

3. **Bluesky Account**
   - Create a dedicated bot account for ATChess
   - Generate an app password at https://bsky.app/settings/app-passwords

## Initial Server Setup

1. **SSH into your server** and run the setup script:
   ```bash
   # Download and run setup script
   wget https://raw.githubusercontent.com/justinabrahms/atchess/main/deploy/scripts/setup-server.sh
   chmod +x setup-server.sh
   sudo DOMAIN=atchess.example.com EMAIL=your@email.com ./setup-server.sh
   ```

2. **Configure environment variables**:
   ```bash
   # Edit protocol service config
   sudo nano /etc/atchess/protocol.env
   
   # Edit web service config  
   sudo nano /etc/atchess/web.env
   ```

3. **Create SSH key for deployments** (on server):
   ```bash
   ssh-keygen -t ed25519 -C "github-actions" -f ~/.ssh/github_actions -N ""
   cat ~/.ssh/github_actions.pub >> ~/.ssh/authorized_keys
   ```

## GitHub Actions Setup

1. **Add secrets** to your GitHub repository (Settings → Secrets and variables → Actions):
   - `DEPLOY_HOST`: Your domain or server IP
   - `DEPLOY_USER`: Your server username (e.g., `root` or created user)
   - `DEPLOY_PORT`: SSH port (usually `22`)
   - `DEPLOY_SSH_KEY`: Contents of `~/.ssh/github_actions` (private key from server)

2. **Deploy** by pushing to main branch or manually triggering the workflow

## Manual Deployment

If you prefer manual deployment:

```bash
# On your local machine
make build
scp bin/atchess-protocol bin/atchess-web user@server:/opt/atchess/bin/
scp -r web/static/* user@server:/var/www/atchess/

# On the server
sudo systemctl restart atchess-protocol atchess-web
```

## Service Management

### Start/Stop Services
```bash
sudo systemctl start atchess-protocol
sudo systemctl start atchess-web
sudo systemctl stop atchess-protocol
sudo systemctl stop atchess-web
```

### View Logs
```bash
# View all logs in tmux
atchess-logs all

# View specific service logs
atchess-logs protocol
atchess-logs web
atchess-logs nginx

# Using journalctl directly
sudo journalctl -u atchess-protocol -f
sudo journalctl -u atchess-web -f
```

### Check Status
```bash
atchess-status
```

## SSL Certificate Renewal

Certbot automatically renews certificates. To manually renew:
```bash
sudo certbot renew --dry-run  # Test renewal
sudo certbot renew             # Force renewal
```

## Monitoring

### Health Checks
- Protocol service: `https://atchess.example.com/health`
- Web UI: `https://atchess.example.com/`

### Resource Usage
```bash
# Check memory and CPU
htop

# Check disk usage
df -h

# Check service resource usage
systemctl status atchess-protocol
systemctl status atchess-web
```

## Troubleshooting

### Services Won't Start
1. Check logs: `sudo journalctl -u atchess-protocol -n 50`
2. Verify binaries are executable: `ls -la /opt/atchess/bin/`
3. Check environment files: `sudo cat /etc/atchess/*.env`

### 502 Bad Gateway
1. Ensure services are running: `atchess-status`
2. Check nginx config: `sudo nginx -t`
3. Verify upstream ports in nginx match service configs

### OAuth Callback Issues
1. Ensure `ATCHESS_PUBLIC_URL` is set correctly in web.env
2. Check nginx is properly forwarding `/callback` endpoint
3. Verify SSL certificate is valid

### Permission Errors
```bash
# Fix ownership
sudo chown -R atchess:atchess /opt/atchess
sudo chown -R atchess:atchess /etc/atchess
sudo chown -R www-data:www-data /var/www/atchess
```

## Security Considerations

1. **Firewall**: Only ports 80, 443, and SSH are open
2. **Fail2ban**: Protects against brute force attacks
3. **SystemD hardening**: Services run with limited privileges
4. **SSL/TLS**: Enforced with HSTS header
5. **Environment files**: Sensitive data stored with restricted permissions

## Backup Strategy

Regular backups should include:
- `/etc/atchess/` - Configuration files
- `/opt/atchess/data/` - Application data (if any)
- Database dumps (if using external database)

Example backup script:
```bash
#!/bin/bash
BACKUP_DIR="/backups/atchess/$(date +%Y%m%d)"
mkdir -p $BACKUP_DIR
cp -r /etc/atchess $BACKUP_DIR/
tar -czf $BACKUP_DIR/atchess-data.tar.gz /opt/atchess/data/
```

## Updates and Maintenance

1. **Update system packages**:
   ```bash
   sudo apt update && sudo apt upgrade -y
   ```

2. **Update ATChess**: Push changes to main branch to trigger automatic deployment

3. **Rotate logs**: Handled automatically by logrotate

## Performance Tuning

For high traffic:

1. **Nginx tuning** (`/etc/nginx/nginx.conf`):
   ```nginx
   worker_processes auto;
   worker_connections 2048;
   keepalive_timeout 65;
   ```

2. **SystemD limits**: Adjust in service files if needed
   ```ini
   LimitNOFILE=65536
   MemoryLimit=1G
   ```

3. **Enable caching**: Uncomment cache directives in nginx config