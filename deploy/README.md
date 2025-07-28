# ATChess Deployment

This directory contains deployment scripts and configurations for ATChess instances.

## Scripts

### `scripts/setup-atchess-abrahms.sh`

Server setup script for the atchess.abrah.ms deployment. This script:

- Creates the `atchess` service user
- Sets up directory structure with secure permissions
- Generates OAuth keys (if they don't exist)
- Configures systemd services
- Sets up log rotation

**Usage:**
```bash
sudo ./deploy/scripts/setup-atchess-abrahms.sh
```

**Security features:**
- OAuth keys stored in `/etc/atchess/keys/` with read-only permissions
- Service runs as non-root user
- Systemd hardening applied
- Proper file permissions throughout

## Creating Your Own Deployment

To create a deployment script for your own domain:

1. Copy `scripts/setup-atchess-abrahms.sh` to `scripts/setup-atchess-yourdomain.sh`
2. Update the `DOMAIN` variable in the script
3. Adjust any other deployment-specific settings
4. Run with sudo on your server

## OAuth Key Management

The deployment scripts handle OAuth key generation automatically. Keys are:
- Generated only if they don't exist (preserving existing keys)
- Stored in `/etc/atchess/keys/` with 400 permissions
- Owned by the service user
- Never overwritten on subsequent deployments

See [OAuth Key Setup](../docs/oauth-key-setup.md) for manual key management.