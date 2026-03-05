# Server Setup for ATChess Deployment

This documents the server-side configuration required for GitHub Actions auto-deploy.

## Overview

- **Server**: abrah.ms (DigitalOcean, Ubuntu 24.04)
- **Web server**: Caddy (reverse proxy + TLS)
- **Services**: `atchess-protocol` (port 8080), `atchess-web` (port 8081)
- **Deploy method**: GitHub Actions pushes binaries via SSH

## Users and Groups

### `atchess` (system user)
- Runs the atchess systemd services
- Shell: `/usr/sbin/nologin` (no interactive login)
- Owns `/srv/atchess/` and all application files

### `atchess-deploy` (deploy user)
- Used by GitHub Actions for SSH-based deployment
- Shell: `/bin/bash`
- Groups: `atchess`, `www-data` (allows writing to app directories)

## Creating the deploy user

```bash
# Create user with membership in atchess and www-data groups
sudo useradd -m -s /bin/bash -G atchess,www-data atchess-deploy

# Set up SSH authorized_keys
sudo mkdir -p /home/atchess-deploy/.ssh
sudo chmod 700 /home/atchess-deploy/.ssh
# Add the public key corresponding to the DEPLOY_SSH_KEY GitHub secret
sudo tee /home/atchess-deploy/.ssh/authorized_keys <<< "<public-key-here>"
sudo chmod 600 /home/atchess-deploy/.ssh/authorized_keys
sudo chown -R atchess-deploy:atchess-deploy /home/atchess-deploy/.ssh
```

## Sudoers configuration

The deploy user needs passwordless sudo for systemctl commands only:

```bash
# /etc/sudoers.d/atchess-deploy
atchess-deploy ALL=(ALL) NOPASSWD: /bin/systemctl stop atchess-protocol, /bin/systemctl stop atchess-web, /bin/systemctl restart atchess-protocol, /bin/systemctl restart atchess-web, /bin/systemctl daemon-reload, /bin/systemctl status atchess-protocol, /bin/systemctl status atchess-web
```

Install with:
```bash
echo 'atchess-deploy ALL=(ALL) NOPASSWD: /bin/systemctl stop atchess-protocol, /bin/systemctl stop atchess-web, /bin/systemctl restart atchess-protocol, /bin/systemctl restart atchess-web, /bin/systemctl daemon-reload, /bin/systemctl status atchess-protocol, /bin/systemctl status atchess-web' | sudo tee /etc/sudoers.d/atchess-deploy
sudo chmod 440 /etc/sudoers.d/atchess-deploy
sudo visudo -c  # validate syntax
```

## Directory structure

```
/srv/atchess/
  app/
    bin/
      atchess-protocol    # protocol service binary
      atchess-web         # web service binary
    config/
      web-config.yaml     # web service config
    web/
      static/             # HTML, CSS, JS assets
      config.yaml -> /srv/atchess/app/config/web-config.yaml
    config.yaml           # protocol service config
    lexicons/             # AT Protocol lexicon definitions
```

All directories under `/srv/atchess/` must be group-writable (`g+w`) so `atchess-deploy` (via `www-data` group membership) can update files during deployment.

```bash
sudo chmod -R g+w /srv/atchess/
```

## GitHub Actions secrets

| Secret | Description |
|--------|-------------|
| `DEPLOY_SSH_KEY` | Private SSH key (ed25519) for `atchess-deploy` user |
| `DEPLOY_HOST` | Server hostname (e.g., `abrah.ms`) |
| `DEPLOY_USER` | SSH username (`atchess-deploy`) |
| `DEPLOY_PORT` | SSH port |

## Generating a new deploy key

If you need to rotate the deploy key:

```bash
# Generate keypair locally
ssh-keygen -t ed25519 -f atchess-deploy-key -N "" -C "atchess-deploy-github-actions"

# Add public key to server
ssh justin@abrah.ms "sudo tee /home/atchess-deploy/.ssh/authorized_keys" < atchess-deploy-key.pub

# Update GitHub secret
gh secret set DEPLOY_SSH_KEY < atchess-deploy-key

# Clean up local files
rm atchess-deploy-key atchess-deploy-key.pub
```

## Systemd services

Service files are located at:
- `/etc/systemd/system/atchess-protocol.service`
- `/etc/systemd/system/atchess-web.service`

These are installed during initial server setup, not by CI/CD.
