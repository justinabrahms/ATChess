# ATChess Caddy Configuration

This directory contains Caddy configuration snippets for ATChess deployments.

## Files

- `atchess.caddy` - Example Caddy configuration for proxying ATChess services

## Critical Routes

The following routes MUST be properly configured for ATChess to work:

1. **`/client-metadata.json`** - OAuth client metadata (required for AT Protocol OAuth)
   - Must be accessible at the root of your domain
   - Proxies to the protocol service at port 8080

2. **`/api/*`** - All API routes including OAuth callbacks
   - Proxies to the protocol service at port 8080
   - Includes WebSocket support for real-time updates

3. **`/`** - Web interface
   - Proxies to the web service at port 8081
   - Must be last to catch all other routes

## Usage

Add the following to your Caddyfile:

```caddy
atchess.yourdomain.com {
    # OAuth client metadata - MUST be accessible at root path
    handle_path /client-metadata.json {
        reverse_proxy localhost:8080
    }

    # API routes (including OAuth callbacks)
    handle_path /api/* {
        reverse_proxy localhost:8080
    }

    # Web interface (must be last to catch all other routes)
    handle {
        reverse_proxy localhost:8081
    }

    # Your existing TLS configuration
    tls {
        dns lego_deprecated dnsimple
    }
    
    # Logging
    log {
        output file /var/log/caddy/atchess.yourdomain.com/access.log {
            roll_keep 3
        }
    }
    
    encode gzip
}
```

## Troubleshooting

### OAuth "Missing code or state" error

This error typically means the `/client-metadata.json` route is not properly configured. The error message from Bluesky will look like:

```
Unable to obtain client metadata for "https://yourdomain.com/client-metadata.json": 404 page not found
```

To fix:
1. Ensure the `handle_path /client-metadata.json` block is present in your Caddyfile
2. Verify the route is accessible: `curl https://yourdomain.com/client-metadata.json`
3. Check that it returns valid JSON with OAuth client metadata
4. Ensure the `client_id` in the response matches your domain

### Testing

After configuring Caddy:

1. Test the configuration: `sudo caddy validate --config /etc/caddy/Caddyfile`
2. Reload Caddy: `sudo systemctl reload caddy`
3. Test the routes:
   ```bash
   # OAuth metadata
   curl https://yourdomain.com/client-metadata.json
   
   # API health check
   curl https://yourdomain.com/api/health
   
   # Web interface
   curl https://yourdomain.com/
   ```

### Caddy v2 Notes

- The `handle_path` directive strips the path prefix before proxying
- The `handle` directive is a catch-all and should be last
- WebSocket support is automatic in Caddy v2's `reverse_proxy`