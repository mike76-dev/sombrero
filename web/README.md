# Sombrero Web UI

The web UI is a React + TypeScript single-page app for managing a Sombrero server through its HTTP API. It covers the same ground as the API: workgroups, accounts, shares, access policies, share connections, host bans, and the server statistics.

It is built into the server binary and served at the API address, so there is nothing separate to run or deploy. Open `http://127.0.0.1:9999` in a browser and enter the API password (`api.password` from `sombrero.yml`) on the **Settings** page.

## Setup wizard

Each of the things above has a page of its own, and the **Setup wizard** page walks through them in the order that gets a client onto a share: workgroup, account, share, connection, access policy. Every step either makes a new one or takes one that is already there, so the wizard is as good for adding an account to an existing setup as for a server with nothing on it yet. It is a way through the same pages, not a separate one: everything it does can be done on the pages themselves, which is also where anything is changed afterwards.

## Pages

### Workgroups

A workgroup is a group of accounts that share a connection to a share, and with it the storage quota of that connection. Create one with or without a name (a named workgroup should only be created, if you're absolutely sure that no other workgroup with that name will ever be created, to avoid name collisions); the UUID is what identifies it, and the page shows it with a copy button. Each workgroup lists its public folders, which every member of the workgroup can see (see [Shared Folders](../README.md#shared-folders) for the details). A public folder can be read-only, in which case only the account that uploaded a file may overwrite or delete it. Deleting a workgroup deletes its accounts too.

### Accounts

Pick a workgroup, then add accounts to it with a username and a password. An account can be a guest account instead, which has no password. A guest reaches only the shares that allow guest access, and there it holds whatever the policies of its workgroup grant it (see [Guest and Anonymous Access](../README.md#guest-and-anonymous-access)). The list shows each account with the shares it can access. Accounts can be deleted one by one, or all at once.

### Shares

A share is a `renterd` bucket or an `indexd` account that clients see as a network drive. For `renterd`, enter the server address, its API password and the bucket. For `indexd`, enter the indexer address and the redundancy as data and parity shards; the account at the indexer is created when a workgroup connects. The remark is free text for your own use.

The list of registered shares shows, for each share, which workgroups are connected and which policies are in place. Two things can be switched on per share: guest access, for the passwordless accounts of a workgroup, and anonymous access, for clients that present no credentials at all and reach only the share's public folder. Anonymous access also has to be allowed in the server config. An `indexd` share offers two maintenance tasks on top: a scan for orphaned slabs, which the share pays for while no file uses them, and a check for fragmented slabs, with the option to repack them (see [Slab Fragmentation](../README.md#slab-fragmentation)).

An access policy grants an account of a workgroup read, write, delete and execute permission on a share.

### Connections

A workgroup gets onto a share by connecting to it. For a `renterd` share, this is a single click of **Connect**. An `indexd` share needs an app key. The first time, click **Request approval**, open the link the page shows and approve the registration with the indexer. The connection then completes on its own, and the new app key is shown once. Keep it, because reconnecting later means pasting it back in. A workgroup that is already connected to another share on the same indexer reuses the key it has there and needs no approval. It can also borrow another workgroup's key, but then both workgroups share one account at the indexer, including its quota.

Connecting takes a while, since the client warms up a connection to every host, and the page shows which phase it is in. **Disconnect** takes the workgroup off the share again.

### Import

Sombrero can take over the data of a `renterd` server or of another `indexd` account. Pick the share to import into and the account the files should belong to, enter the address and the credentials of the source, and click **Start**. The import runs on the server, so you can leave the page and come back to it later; the page finds the running import again on its own.

If the source is an account on the same indexer, the files are pinned rather than copied. Pinning means that the share's account becomes an owner of the existing objects and pays for them from now on. The data itself is not touched, so this is quick and costs no bandwidth. In all other cases the data has to be copied, i.e. downloaded from the source and uploaded again. `renterd` encrypts its slabs differently, so they cannot be pinned, and an indexer only knows about its own slabs, so data from another indexer has to be copied too. The page tells you for each file which of the two happened. If the indexer refuses to pin a file, the file is copied instead.

**Check the source** connects to the source and tells you what an import would bring over, before you start one. For an `indexd` account, it also tells you whether the slabs carry their file names (see [Slab Metadata](../README.md#slab-metadata)); if they don't, the import can only put them into lost and found.

#### Lost and found

Slabs whose file names cannot be recovered end up as files in `/lost+found`, one file per slab, named after the slab. They hold the data, but nothing says what it was.

You can try to sort these files out with the button at the bottom of the page. The server downloads each slab and looks for files it can recognize by their structure: JPEG, PNG, GIF, HEIC and WebP images, MP4, MOV, M4A, AVI and WAV media, PDFs, and ZIP archives (this includes Office documents). Each file found is placed into `/lost+found/recovered`, named after its position in the slab. No data is uploaded in this process, because a recovered file simply refers to the data that is already in the slab. The recovered part is removed from the slab's file in `/lost+found`, so that folder shrinks to what could not be recognized. If everything in a slab is recognized, its file disappears from `/lost+found`.

Sorting takes a while, since every slab has to be downloaded. It goes in short rounds, and if it is interrupted, clicking the button again continues where it stopped.

Files larger than a slab cannot be recovered this way. Their parts are spread over several slabs, and there is no way to tell which parts belong together. Once you don't need anything in `/lost+found` anymore, delete it. The slabs will be unpinned, and you will stop paying for them.

### Bans

The server bans hosts that misbehave, and you can ban a host yourself by its IP address, with an optional reason. The page checks whether a host is banned, lifts a single ban, or clears all bans at once, including the ones the server placed itself.

### Stats

Server statistics across all shares: version, uptime, sessions, data transferred, password and permission errors, how much data is waiting to be uploaded, and how much database space the upload buffers take (see [Upload Backlog](../README.md#upload-backlog)).

### Settings

The address and the password the UI uses to reach the API. The address is `/api` when the UI is served by the server itself, which is the normal case.

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

Open the printed URL, go to **Settings**, enter the API password, and click **Save & test connection**.

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

* Connecting a workgroup to a share runs in the background, and the Connections page polls `GET /connect/:workgroup/:share` to show which phase it is in. A first-time indexd connection needs no second click: approving the registration with the indexer is what carries it through.
* When a workgroup connects to an indexd share for the first time, the app key it derives is shown once. Store it safely; it is required for reconnecting.
