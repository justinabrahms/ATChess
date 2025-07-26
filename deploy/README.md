# ATChess Deployment Files

This directory contains all files needed for deploying ATChess to production.

## Directory Structure

```
deploy/
├── systemd/              # SystemD service files
│   ├── atchess-protocol.service
│   └── atchess-web.service
├── nginx/                # Nginx configuration
│   └── atchess.conf
├── scripts/              # Deployment and setup scripts
│   ├── setup-server.sh   # Initial server setup
│   ├── deploy.sh         # Manual deployment script
│   └── update-config.sh  # Configuration update helper
├── .env.production.example  # Example environment configuration
└── DEPLOYMENT.md         # Detailed deployment guide
```

## Quick Start

1. **Set up a new server**: See [DEPLOYMENT.md](DEPLOYMENT.md)
2. **Configure GitHub Actions**: Add secrets to your repository
3. **Deploy**: Push to main branch or run manually

## Files Overview

### SystemD Services
- `atchess-protocol.service` - Runs the AT Protocol backend (port 8080)
- `atchess-web.service` - Runs the web UI server (port 8081)

### Nginx Configuration
- `atchess.conf` - Reverse proxy config with SSL/HTTPS support

### Scripts
- `setup-server.sh` - Installs dependencies and configures a fresh Ubuntu server
- `deploy.sh` - Manual deployment script for direct server deployments
- `update-config.sh` - Updates configuration on existing deployments

### Environment Configuration
- `.env.production.example` - Template for production environment variables

## Security Notes

- All services run as unprivileged `atchess` user
- SystemD services use security hardening options
- Nginx configured with security headers
- SSL/TLS enforced with Let's Encrypt
- Firewall configured to only allow necessary ports