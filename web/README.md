# Sombrero Web GUI

A React + TypeScript single-page app for managing a Sombrero server through its HTTP API:
workgroups, accounts, shares, access policies, share connections, and host bans.

## Development

```bash
cd web
npm install
npm run dev
```

The dev server proxies requests from `/api` to the Sombrero API at
`http://localhost:9999` (the API does not send CORS headers, so the browser cannot
call it cross-origin directly). If the API runs elsewhere:

```bash
SOMBRERO_API_URL=http://my-server:9999 npm run dev
```

Open the printed URL, go to **Settings**, enter the API password
(`api.password` from `sombrero.yml`), and press **Save & test connection**.

## Production build

```bash
npm run build
```

The static site is emitted to `web/dist`. Serve it with any web server and route
`/api/*` to the Sombrero API port, stripping the `/api` prefix. Example Apache
config (requires `a2enmod proxy proxy_http`):

```apache
<VirtualHost *:80>
    DocumentRoot /path/to/sombrero/web/dist

    <Directory /path/to/sombrero/web/dist>
        Require all granted
        FallbackResource /index.html
    </Directory>

    ProxyPass /api/ http://127.0.0.1:9999/
    ProxyPassReverse /api/ http://127.0.0.1:9999/
</VirtualHost>
```

Alternatively, set the full API URL (e.g. `http://localhost:9999`) in **Settings**;
this only works when the request is not subject to CORS restrictions.

## Notes

* When a workgroup connects to an indexd share for the first time, the returned
  app key is shown once — store it safely; it is required for reconnecting.
