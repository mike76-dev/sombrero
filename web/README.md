# Sombrero Web UI

The web UI is a React + TypeScript single-page app for managing a Sombrero server through its HTTP API. It covers the same ground as the API: workgroups, accounts, shares, access policies, share connections, host bans, and the server statistics.

It is built into the server binary and served at the API address, so there is nothing separate to run or deploy. Open `http://127.0.0.1:9999` in a browser and enter the API password (`api.password` from `sombrero.yml`) on the **Settings** page.

## Setup wizard

Each of the things above has a page of its own, and the **Setup wizard** page walks through them in the order that gets a client onto a share: workgroup, account, share, connection, access policy. Every step either makes a new one or takes one that is already there, so the wizard is as good for adding an account to an existing setup as for a server with nothing on it yet. It is a way through the same pages, not a separate one: everything it does can be done on the pages themselves, which is also where anything is changed afterwards.

## Building the UI

The UI is not built as part of `go build`. Release binaries are built by building the UI first and then the server:

```bash
npm --prefix web install
npm --prefix web run build
go build .
```

A server built without this step runs normally and serves the API as usual; only the UI is missing, and it says so if you open it in a browser.

## Development

```bash
cd web
npm install
npm run dev
```

The dev server proxies requests from `/api` to the Sombrero API at `http://localhost:9999` (the API does not send CORS headers, so the browser cannot call it cross-origin directly). If the API runs elsewhere:

```bash
SOMBRERO_API_URL=http://my-server:9999 npm run dev
```

Open the printed URL, go to **Settings**, enter the API password, and press **Save & test connection**.

## Serving the UI separately

`npm run build` emits the static site to `web/dist`. The server embeds it, but it can also be served by any web server that routes `/api/*` to the Sombrero API port, stripping the `/api` prefix. Example Apache config (requires `a2enmod proxy proxy_http`):

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

Alternatively, set the full API URL (e.g. `http://localhost:9999`) in **Settings**; this only works when the request is not subject to CORS restrictions.

## Notes

* Connecting a workgroup to a share runs in the background, and the Connections page polls `GET /connect/:workgroup/:share` to show which phase it is in. A first-time indexd connection needs no second press: approving the registration with the indexer is what carries it through.
* When a workgroup connects to an indexd share for the first time, the app key it derives is shown once. Store it safely; it is required for reconnecting.
