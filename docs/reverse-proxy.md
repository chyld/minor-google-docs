# Running behind a reverse proxy (HTTPS)

Notes listens on plain HTTP, port 4444. To serve it over HTTPS, put a proxy in front and tell Notes
to trust that proxy:

```yaml
# compose.yaml
environment:
  NOTES_TRUSTED_PROXIES: "172.16.0.0/12"   # the proxy's IP or Docker network
  NOTES_SECURE_COOKIES: "auto"             # Secure cookies when the proxy says https
```

The trusted-proxy setting is required. Without it, Notes ignores `X-Forwarded-For` and
`X-Forwarded-Proto`. Login rate limiting would then see every request as coming from the proxy, and
cookies wouldn't be marked Secure.

Uploads have no size limit. Every proxy must allow **unlimited request bodies** and must
**not buffer uploads**. Uploads are sent in 8 MiB chunks, so even Cloudflare's 100 MB limit is fine.

## Caddy

```caddy
notes.example.lan {
    reverse_proxy 127.0.0.1:4444 {
        flush_interval -1
        transport http {
            read_timeout 1h
            write_timeout 1h
        }
    }
    header Strict-Transport-Security "max-age=31536000"
}
```

Caddy has no default body-size limit, so you don't need to change one.

## nginx

```nginx
server {
    listen 443 ssl http2;
    server_name notes.example.lan;
    # ssl_certificate ...; ssl_certificate_key ...;

    client_max_body_size 0;          # no upload limit
    proxy_request_buffering off;     # stream uploads straight through
    proxy_buffering off;
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;

    location / {
        proxy_pass http://127.0.0.1:4444;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;
    }
    add_header Strict-Transport-Security "max-age=31536000" always;
}
```

## Traefik (labels)

```yaml
labels:
  - traefik.enable=true
  - traefik.http.routers.notes.rule=Host(`notes.example.lan`)
  - traefik.http.routers.notes.tls=true
  - traefik.http.services.notes.loadbalancer.server.port=4444
```

Traefik doesn't buffer requests unless you add a buffering middleware, so don't add one.

If you only use the proxy, you can stop exposing 4444 on every interface. Change the port mapping
in `compose.yaml` to `127.0.0.1:4444:4444`.
