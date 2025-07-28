# ATChess Nginx Configuration

This directory contains nginx configuration snippets for ATChess deployments.

## Files

- `atchess.conf` - Main nginx configuration snippet for proxying ATChess services

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

### Option 1: Include the config file

In your nginx server block:

```nginx
server {
    server_name atchess.yourdomain.com;
    
    # ... SSL configuration ...
    
    include /etc/atchess/nginx/atchess.conf;
}
```

### Option 2: Copy the location blocks

Copy the contents of `atchess.conf` directly into your server block.

## Troubleshooting

### OAuth "Missing code or state" error

This error typically means the `/client-metadata.json` route is not properly configured. Verify:

1. The route is accessible: `curl https://yourdomain.com/client-metadata.json`
2. It returns valid JSON with OAuth client metadata
3. The `client_id` in the response matches your domain

### WebSocket connection failures

Ensure the `/api/` location includes the WebSocket headers:

```nginx
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection "upgrade";
```

## Testing

After configuring nginx:

1. Test the configuration: `sudo nginx -t`
2. Reload nginx: `sudo nginx -s reload`
3. Test the routes:
   ```bash
   # OAuth metadata
   curl https://yourdomain.com/client-metadata.json
   
   # API health check
   curl https://yourdomain.com/api/health
   
   # Web interface
   curl https://yourdomain.com/
   ```