<img src="logo.png" width="128">

# Sombrero
This is an SMB server integrated into the Sia decentralized cloud storage. Users can connect to it from their PCs and access the Sia storage like they would normally do with a regular remote drive.

## Prerequisites
* At least one `renterd` or `indexd` node running either locally or on a remote machine is required. The node needs to be funded, have the minimal required number of active storage contracts, and be accessible from the machine where the server is running.
Even though it is possible to use a single or multiple remote `renterd` nodes, it is recommended to run the node locally, to avoid an overhead caused by the additional Internet traffic.
How to set up a `renterd` node is described here: [https://github.com/SiaFoundation/renterd](https://github.com/SiaFoundation/renterd).
The setup process of an `indexd` node is described here: [https://github.com/SiaFoundation/indexd](https://github.com/SiaFoundation/indexd). Alternatively, one can connect to the [Sia Foundation indexer](https://sia.storage/).
* The SMB port 445 needs to be open on the machine where the server is running.

## Installing on Ubuntu
On Ubuntu, the installer script does the whole setup: it installs the server, sets PostgreSQL up in the Normal mode, writes the configuration, and runs the server as a systemd service under a user of its own.
```Bash
curl -fsSLO https://raw.githubusercontent.com/mike76-dev/sombrero/master/install-sombrero.sh
sudo bash install-sombrero.sh
```
It asks for the mode and an API password, and prints how to reach the web UI when it is done. Run it again to upgrade the server to the latest release; the configuration and the data are kept. `sudo bash install-sombrero.sh --help` lists the options.

The sections below describe the same setup by hand, which is also what to follow on the other systems.

## Installing PostgreSQL
This section will assume you are running Ubuntu Server 24.04. On the other systems, the commands may be different.

The default Ubuntu repositories ship an older PostgreSQL version, so add the official PostgreSQL (PGDG) repository first:
```Bash
sudo apt install postgresql-common -y
sudo /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y
```
Then install PostgreSQL 18:
```Bash
sudo apt install postgresql-18 postgresql-contrib -y
```
Verify the installation:
```Bash
sudo systemctl status postgresql
```
You should see something like:
```
● postgresql.service - PostgreSQL RDBMS
     Loaded: loaded (/lib/systemd/system/postgresql.service; enabled)
     Active: active (exited)
```
If it is inactive, start and enable it:
```Bash
sudo systemctl enable --now postgresql
```
PostgreSQL creates a Unix user named postgres. Switch to it:
```Bash
sudo -i -u postgres
```
Then open the PostgreSQL shell:
```Bash
psql
```
You should see something like:
```
psql (18.x)
Type "help" for help.

postgres=#
```
Inside the `psql` prompt:
```SQL
CREATE DATABASE <DATABASE>;
CREATE USER <USER> WITH ENCRYPTED PASSWORD '<DB_PASSWORD>';
GRANT ALL PRIVILEGES ON DATABASE <DATABASE> TO <USER>;
\c <DATABASE>
GRANT USAGE ON SCHEMA public TO <USER>;
GRANT CREATE ON SCHEMA public TO <USER>;

```
Take a note of `<DATABASE>`, `<USER>`, and `<DB_PASSWORD>`, as you will need these values later on.
Exit `psql` with:
```SQL
\q
```

## Running the Server
A config file, `sombrero.yml`, needs to be created in the directory where the server will be running. It should contain the following lines:
```YAML
debug: false               # indicates whether to display the session ID and key for tools like Wireshark to decrypt the encrypted data
mode: normal               # the server mode: 'normal' or 'lite' (see below)
maxConnections: 30         # the maximum number of connections accepted from the same IP within 10 minutes
anonymous: false           # optional: whether clients presenting no credentials at all are admitted; a share
                           # has to offer it as well (see below). If omitted, they are turned away
api:
  address: 127.0.0.1:9999  # the address the API is listening on; defaults to localhost, since the API administers
                           # the whole server. Change it only if the API needs to be reached from another machine,
                           # and put a reverse proxy with TLS in front of it if you do
  password: <API_PASSWORD> # the password to access the API; the server refuses to start without one
database:
  host: 127.0.0.1          # the address of the PostgreSQL server
  port: 5432               # the port number of the PostgreSQL server
  user: <USER>             # the name of the database user from the previous section
  password: <DB_PASSWORD>  # the password of the database user from the previous section; should be at least 4 characters long
  database: <DATABASE>     # the name of the PostgreSQL database from the previous section
  sslMode: disable         # the SSL mode of the PostgreSQL server
indexd:
  appName: Sombrero                                                              # the name of the app, unique to the `indexd` node being connected to
  description: Sombrero SMB server                                               # description of the app
  logoURL: https://raw.githubusercontent.com/mike76-dev/sombrero/master/logo.png # URL of the app logo, can be left as it is
  serviceURL: https://github.com/mike76-dev/sombrero                             # URL of the app itself, can be left as it is (Sombrero has no service page)
  seedPhrase: ''                                                                 # if omitted, the server will generate a new seed phrase and put it here
  maxBufferAge: never                                                            # optional: how long the data that does not fill a slab may wait to be packed
                                                                                 # with the data of other files; if omitted, it waits indefinitely
  minPackedSlabSize: 0                                                           # optional: the least amount of leftover data, in bytes, that an incomplete slab
                                                                                 # is uploaded with once it has reached maxBufferAge; if omitted, any amount is uploaded
  fragmentationThreshold: 0.25                                                   # optional: how much of a slab may be dead space before it is reported, as a fraction
                                                                                 # between 0 and 1; if omitted, defaults to 0.25
  fragmentationCheck: 1h                                                         # optional: how often to look for the dead space; 'never' turns the check off and leaves
                                                                                 # it to the web UI and the API to report on demand
  defragment: false                                                              # optional: whether the check also repacks the slabs it reports, instead of only reporting
                                                                                 # them; if omitted, nothing is repacked on its own
  maxBufferedData: 0                                                             # optional: the most data, in bytes, that all shares may keep in the database waiting
                                                                                 # to be uploaded before clients' writes are held back; if omitted, there is no limit
```
The server can be started either as a standalone executable or as a service (the latter is preferred). For example, on Linux:
```Bash
sudo sombrero --dir=<PATH_TO_SOMBRERO.YML>
```
The superuser access is required because of the port 445 that the server is listening on.

Now, you need to register shares and add user accounts that will be accessing these shares.
This can be done either from the web UI, which is served at the API address (`http://127.0.0.1:9999` by default), or with the API calls described below. The typical workflow is:

### 1. Create a workgroup
A workgroup can contain an arbitrary number of user accounts. Each workgroup can connect to a remote share and have its own storage quota on that share.
```Bash
curl -u "":<API_PASSWORD> -X POST "http://127.0.0.1:9999/api/workgroup"
```
or
```Bash
curl -u "":<API_PASSWORD> -X POST "http://127.0.0.1:9999/api/workgroup" -d '{"name":"home"}'
```
The difference between the two calls is that the second call allows creating a named workgroup.
This is only useful when you a running a private server and know for sure that no other workgroup with the same name will ever be created.

Example of the output:
```Bash
{"uuid":"8303eeb8-f30e-4607-9eb7-875df2c5bd52"}
```

### 2. Add user account(s) to the workgroup
```Bash
curl -u "":<API_PASSWORD> -X POST "http://127.0.0.1:9999/api/account" -d '{"username":"test","password":"123","workgroup":"8303eeb8-f30e-4607-9eb7-875df2c5bd52"}'
```
or
```Bash
curl -u "":<API_PASSWORD> -X POST "http://127.0.0.1:9999/api/account" -d '{"username":"test","password":"123","workgroup":"home"}'
```

### 3. Register a share
```Bash
curl -u "":<API_PASSWORD> -X POST "http://127.0.0.1:9999/api/share" -d '{"name":"shared-renterd","type":"renterd","serverName":"http://127.0.0.1:9980","password":"1234","bucket":"default","remark":"renterd"}'
```
or
```Bash
curl -u "":<API_PASSWORD> -X POST "http://127.0.0.1:9999/api/share" -d '{"name":"shared-indexd","type":"indexd","serverName":"https://sia.storage","remark":"Sia Foundation indexer","dataShards":10,"parityShards":20}'
```

### 4. Connect the workgroup to the share
Connecting takes a while — the client warms up a connection to every host, and an `indexd`
share waits for a person to approve the registration first — so the calls that start a
connection return right away and the connection is made in the background. `GET` reports how
far along it is: `awaiting-approval`, `registering` or `connecting` while it runs, and
`connected` or `failed` when it is over.

In case of a `renterd` share, simply call
```Bash
curl -u "":<API_PASSWORD> -X PUT "http://127.0.0.1:9999/api/connect/home/shared-renterd"
```
Connecting to an `indexd` share is slightly more involved. First, request a connection:
```Bash
curl -u "":<API_PASSWORD> -X POST "http://127.0.0.1:9999/api/connect/8303eeb8-f30e-4607-9eb7-875df2c5bd52/shared-indexd"
```
Example of the output:
```Bash
{"state":"awaiting-approval","started":"2026-09-08T10:15:04Z","since":"2026-09-08T10:15:04Z","url":"https://sia.storage/auth/connect/d10f2a960d7dfc947248f58758619b74"}
```
Visit the URL provided and accept the connection. That is all it takes: the server picks the
approval up and connects the share. Follow it with
```Bash
curl -u "":<API_PASSWORD> "http://127.0.0.1:9999/api/connect/8303eeb8-f30e-4607-9eb7-875df2c5bd52/shared-indexd"
```
Example of the output once it is done:
```Bash
{"state":"connected","started":"2026-09-08T10:15:04Z","since":"2026-09-08T10:15:41Z","appKey":"03a2aab52b79f674354af35b0030cd0cd45b51f53a1a75795a58c85844767b3d3ac38242c05637cac5b8b7fbcea55d29826845fdfc0ef19894d7640438f43a22"}
```
Keep that app key: it is what reconnects this workgroup to the share, and it is reported
once and never again.
```Bash
curl -u "":<API_PASSWORD> -X PUT "http://127.0.0.1:9999/api/connect/8303eeb8-f30e-4607-9eb7-875df2c5bd52/shared-indexd" -d '{"appKey":"03a2aab5..."}'
```

### 5. Grant access to the share
To grant an account access to the share, run:
```Bash
curl -u "":<API_PASSWORD> -X PUT "http://127.0.0.1:9999/api/share/shared-indexd/policy?username=test&workgroup=8303eeb8-f30e-4607-9eb7-875df2c5bd52&read=true&write=true&delete=true&execute=true"
```

## Web UI
The server ships with a web UI covering the same ground as the API: workgroups, accounts, shares, access policies, bans, and the server statistics. It is built into the binary and served at the API address, so there is nothing separate to run or deploy. Open `http://127.0.0.1:9999` in a browser and enter `<API_PASSWORD>` on the Settings page.

The **Setup wizard** page walks through the workflow above — workgroup, account, share, connection, access policy — one step at a time, making each thing or letting you pick one that is already there. It is a way through the same pages, not a separate one: everything it does can be done on the pages themselves, which is also where anything is changed afterwards.

### Building the UI
The UI is not built as part of `go build`. Release binaries are built by building the UI first and then the server:
```Bash
npm --prefix web install
npm --prefix web run build
go build .
```
A server built without this step runs normally and serves the API as usual; only the UI is missing, and it says so if you open it in a browser.

## Running in Docker
The server can also run in a container. The image is published for `amd64` and `arm64` as `ghcr.io/mike76-dev/sombrero`, tagged with the version and with `latest`. It is the same for both modes, since the mode is read from `sombrero.yml` at startup, and it has the web UI built in. To build it from the source instead, run in the repository root:
```Bash
docker build -t ghcr.io/mike76-dev/sombrero .
```
The container runs on the host network, so the instructions below assume a Linux host. This way the server sees the real addresses of its clients, which the bans rely on, and offers them the host's network interfaces for multichannel rather than the container's. It also means the API listens wherever `api.address` says, exactly as without Docker: `127.0.0.1:9999` keeps it reachable from the host alone.

The data directory is mounted at `/data` in the container. Put `sombrero.yml` there before the first start. The server runs as root inside the container, so the files it creates there, such as `store.json`, belong to root.

### Running in the [Lite mode](#lite-mode)
With `mode: lite` in `data/sombrero.yml`, nothing else is needed:
```Bash
docker run -d --name sombrero --network host --restart unless-stopped --stop-timeout 60 -v ./data:/data ghcr.io/mike76-dev/sombrero
```
`--stop-timeout` gives the server time to finish the uploads in flight when the container is stopped. To upgrade, pull the new image and replace the container; the data directory stays as it is:
```Bash
docker pull ghcr.io/mike76-dev/sombrero
docker rm -f sombrero
```
and then run the `docker run` command above again.

### Running in the Normal mode
`compose.yaml` runs the server together with PostgreSQL, which listens on the host's `127.0.0.1` only. Create a `.env` file next to it with the database password, and the port if 5432 is already taken, e.g. by a PostgreSQL installed on the host:
```
SOMBRERO_DB_PASSWORD=<DB_PASSWORD>
SOMBRERO_DB_PORT=5432
```
The `database` section of `data/sombrero.yml` has to match:
```YAML
database:
  host: 127.0.0.1
  port: 5432               # SOMBRERO_DB_PORT
  user: sombrero
  password: <DB_PASSWORD>  # SOMBRERO_DB_PASSWORD
  database: sombrero
  sslMode: disable
```
Then start both containers:
```Bash
docker compose up -d
```
The database is kept in the `postgres` volume. To follow the server log, run `docker compose logs -f sombrero`; to stop both containers, `docker compose down`. To upgrade:
```Bash
docker compose pull
docker compose up -d
```

## Upload Packing
A file whose size is not a multiple of the slab size leaves a piece of data behind that is too small for a slab of its own. Such pieces are kept in the database until they can be packed together into a full slab, which is uploaded as one. By default they are kept for as long as that takes, because an incomplete slab occupies as much storage as a full one. Both config fields are optional: setting `maxBufferAge` (for example, `24h`) uploads them anyway once they have waited that long, while `minPackedSlabSize` (for example, `1048576`) holds that upload back until the leftover data of a share is worth a slab. On its own, `minPackedSlabSize` has no effect.

## Upload Backlog
What clients write to an `indexd` share is stored in the database first and uploaded to the network in the background. If clients write faster than the network takes the data, the backlog grows until the database runs out of disk space. Setting `maxBufferedData` caps the backlog across all shares:

- Once half of it is used, clients are no longer allowed more writes in flight than they already have, so they stop speeding up.
- At the limit, writes wait for the backlog to drain, which is what holds the client back: it is left waiting for the answers it needs before it can send more. A write that waits for more than 5 minutes fails: the client reports that the disk is full, and the file being written is not stored.
- While the backlog is at the limit, the leftover pieces waiting to be packed are uploaded straight away, even if they don't fill a slab, as though `maxBufferAge` had passed.

Uploading frees the data in the database, but PostgreSQL reclaims the disk space only when it vacuums the table. Even then the table keeps its largest size and reuses the space instead of returning it. The disk the database uses therefore grows to about `maxBufferedData` plus whatever the vacuum has not reclaimed yet, so set it to about half the free space on that disk. The server logs a warning when the table takes more than twice `maxBufferedData` and at least 64 MiB more than the data it holds, which means the vacuum is falling behind.

The web UI reports both figures on its Statistics page, across all shares: how much is waiting to be uploaded right now, and how much database space the buffers occupy. The second one grows to the largest backlog the server has ever held and stays there, even once everything has been uploaded, because the space is reused rather than given back.

The limit only applies to `indexd` shares, so it has no effect in the [Lite mode](#lite-mode). `renterd` keeps its own upload cache, which Sombrero cannot see.

## Slab Fragmentation
Deleting or overwriting a file punches a hole in the slab it was packed into, and the share keeps paying for the whole slab. A slab belongs to the workgroup that uploaded it, so each workgroup's connection to the share looks for it in its own slabs every `fragmentationCheck` (`1h` by default, `never` to turn the check off) and reports the slabs that are at least `fragmentationThreshold` dead space (`0.25` by default).

Setting `defragment: true` has the check repack what it reports instead of only reporting it: what is still referenced in those slabs is downloaded, put back into the upload queue to be packed together with the data of other files, and the slabs it came out of are unpinned once nothing reads from them any more.

Repacking costs what any other upload of the same data costs, and between a round and the packed slab that follows it the moved data sits in the database rather than on the network. Rounds give way to what clients are writing: one only starts while less than a slab's worth of data is waiting to be uploaded, and `maxBufferAge` is what bounds how long the moved data waits there.

## Shared Folders
It is possible to define a list of shared folder names for each workgroup. Files uploaded or moved to such folders are not only visible for those users who uploaded or moved them, but for all members of the workgroup. Only working on `indexd` shares.

To create such list, run:
```Bash
curl -u "":<API_PASSWORD> -X PUT "http://127.0.0.1:9999/api/workgroup/8303eeb8-f30e-4607-9eb7-875df2c5bd52" -d '{"publicDirs":[{"path":"Public"},{"path":"Reports","readOnly":true,"caseSensitive":true}]}'
```
Every entry has the following fields:

| Field | Default | Description |
| --- | --- | --- |
| `path` | | The name of the shared folder. |
| `readOnly` | `false` | If `true`, a file in the folder may only be overwritten, renamed over, or deleted by the account that placed it there. The other members of the workgroup can only read it. If `false`, any member of the workgroup may rewrite or delete any file in the folder. |
| `caseSensitive` | `false` | If `true`, a folder is only shared when its name matches `path` exactly. |

The call replaces the whole list, so passing an empty list removes all shared folders of the workgroup. If the same folder name appears in the list more than once, only the first entry is kept.

Changing the list also applies to the folders that already exist: a folder whose name starts matching an entry becomes shared, a folder that no longer matches any entry becomes private again, and a folder that stays matched picks up the new flags.

## Guest and Anonymous Access
A share is reached only by the accounts its policies name, unless it is told to offer one of the two ways in below. Those are two different concepts, and both are turned off by default.

A **guest** is an account of a workgroup that has no password, so anybody who knows its name may log in as it. In every other way it is an ordinary account: it belongs to a workgroup, the policies of the share decide what it may do, and the files it uploads belong to it. Create one by leaving the password empty:
```Bash
curl -u "":<API_PASSWORD> -X POST "http://127.0.0.1:9999/api/account" -d '{"username":"guest","password":"","workgroup":"8303eeb8-f30e-4607-9eb7-875df2c5bd52"}'
```
Such an account is refused while no share offers guest access, since it would have nowhere to connect. A share offers it from its details with:
```Bash
curl -u "":<API_PASSWORD> -X PUT "http://127.0.0.1:9999/api/share/<SHARE_NAME>" -d '{"allowGuest":true}'
```

An **anonymous** client presents no credentials at all. This is turned off for the whole server unless `anonymous: true` is set in the config file. A share then offers it together with the folder to confine it to:
```Bash
curl -u "":<API_PASSWORD> -X PUT "http://127.0.0.1:9999/api/share/<SHARE_NAME>" -d '{"allowAnonymous":true,"publicDir":"public"}'
```
An anonymous session reaches that folder and nothing else: what it calls the root of the share is the folder itself, so the rest of the share has no name it could ask for. What it drops there every member of the share can see, while what they keep elsewhere stays as private from it as it is from each other. Everything in the folder belongs to one identity, so one anonymous client may delete what another dropped; the folder is public in both directions.

On an `indexd` share the folder is served by a connection of its own, made once under the reserved workgroup `00000000-0000-0000-0000-000000000000`:
```Bash
curl -u "":<API_PASSWORD> -X POST "http://127.0.0.1:9999/api/connect/00000000-0000-0000-0000-000000000000/<SHARE_NAME>"
```
The call returns a URL to approve, exactly as for any other workgroup. What anonymous uploads is then pinned under an app account of its own, with its own quota, so no workgroup pays for it. The server makes the folder itself; if a folder of that name is already there and belongs to somebody else, the share turns anonymous sessions away and says so in the log rather than handing that folder to everyone.

## Lite Mode
If you only intend to connect to `renterd` shares, you can run the server in the Lite mode by setting `mode: lite` in the config file. In this mode, no PostgreSQL database is required: the shares, workgroups, accounts, access policies, and the ban list are kept in a JSON file (`store.json`) in the data directory, and the `database` and `indexd` sections of the config file may be omitted. `indexd` shares are not supported in the Lite mode.

## Security Considerations
An open TCP port 445 attracts thousands of attackers and those who look for a free storage. For this reason, guest and anonymous accesses are turned off by default. Even when the server is running on a private LAN, it should not be a problem to create a password-protected account like described above; a share that admits anybody is worth a deliberate decision, and [Guest and Anonymous Access](#guest-and-anonymous-access) describes what each of them can reach.

The API administers the whole server, so it listens on `127.0.0.1` unless the config file says otherwise, and the server refuses to start without an API password. Repeated failed logins from the same host are throttled. If you do need to reach the API or the web UI from another machine, prefer an SSH tunnel:
```Bash
ssh -N -L 9999:127.0.0.1:9999 <USER>@<SERVER_NET_ADDRESS>
```
Binding the API to a public interface exposes the password over plain HTTP; put a reverse proxy with TLS in front of it if you go that way, and see [Running Behind a Reverse Proxy](#running-behind-a-reverse-proxy) below.

The server also has a built-in abuse protection. If 30 or more connections are detected from the same IP address within 10 minutes, this IP is permanently banned. This number of 30 can be configured in the config file (see above).

Also banned are those remote hosts, which continue sending SMB1 requests after receiving the initial SMB2 response from the server.

The bans are saved in the database, and the reason for the ban is provided. If a host ends up banned by mistake, it can be removed manually:
```Bash
curl -u "":<API_PASSWORD> -X DELETE "http://127.0.0.1:9999/api/ban/<IP_OF_THE_REMOTE_HOST>"
```

## Running Behind a Reverse Proxy
The API and the web UI are served over plain HTTP, so reaching them from another machine calls for a reverse proxy that terminates TLS. The proxy has to run on the same machine as the server, and the API has to keep listening on `127.0.0.1:9999`, so that the proxy is the only way in. Below are the recommendations for Apache2.

Enable the modules the setup needs:
```Bash
sudo a2enmod proxy proxy_http ssl headers
```

Then create a virtual host, e.g. in `/etc/apache2/sites-available/sombrero.conf`:
```ApacheConf
<VirtualHost *:80>
    ServerName sombrero.example.com
    Redirect permanent / https://sombrero.example.com/
</VirtualHost>

<VirtualHost *:443>
    ServerName sombrero.example.com

    SSLEngine on
    SSLCertificateFile    /etc/letsencrypt/live/sombrero.example.com/fullchain.pem
    SSLCertificateKeyFile /etc/letsencrypt/live/sombrero.example.com/privkey.pem

    RequestHeader unset X-Forwarded-For

    ProxyPreserveHost On
    ProxyPass        / http://127.0.0.1:9999/
    ProxyPassReverse / http://127.0.0.1:9999/
</VirtualHost>
```

Enable the site and reload:
```Bash
sudo a2ensite sombrero
sudo apache2ctl configtest
sudo systemctl reload apache2
```

If Apache is itself behind another proxy or a CDN, configure [mod_remoteip](https://httpd.apache.org/docs/current/mod/mod_remoteip.html) with the addresses of those front-ends, so that the entry Apache appends is the real client rather than the hop in front of it.

## Connecting to the Server
In the guides below, `<SERVER_NET_ADDRESS>` stands for the network address of the SMB server, while `<SHARE_NAME>` is the name of the share registered earlier.

### Windows
1. Right-click on `This PC` icon and choose `Map network drive...` from the popup menu.
2. Type the address of the share in the `Folder` field (`\\<SERVER_NET_ADDRESS>\<SHARE_NAME>`). Pick any drive letter. Check the `Connect using different credentials` box, then click `Finish`.
3. In the next popup window, enter the user credentials (matching one of the registered accounts) and click `OK`.

If Windows doesn't offer you to specify the workgroup name, simply enter `<WORKGROUP>\<USERNAME>` as the username.

Please note: Windows 2000/NT/XP and earlier are not supported. The earliest supported versions are Windows 7/Vista, because this is where the SMB2 protocol was first introduced.

### MacOS
1. In the `Finder` menu choose `Go -> Connect to Server...`.
2. Enter `smb://<SERVER_NET_ADDRESS>/<SHARE_NAME>` as the server name, then click `Connect`.
3. In the next popup window, choose `Connect As: Registered User` and enter the user credentials (matching one of the registered accounts, in form `<WORKGROUP>`\\`<USERNAME>`), then click `Connect`.

### Ubuntu GUI
1. In the file manager (e.g. Nautilus), navigate to `Other Locations`, enter `smb://<SERVER_NET_ADDRESS>/<SHARE_NAME>` in the `Enter server address` field, then click `Connect`.
2. In the next popup window, choose `Connect As: Registered User` and enter the user credentials (matching one of the registered accounts), then click `Connect`.

### Ubuntu CLI
1. If needed, install `cifs-utils` with
```Bash
sudo apt install cifs-utils
```
2. Create a mount path with
```Bash
sudo mkdir /mnt/sia
```
and change the ownership with
```Bash
sudo chown $USER:$USER /mnt/sia
```
3. Mount the share with
```Bash
sudo mount -t cifs //<SERVER_NET_ADDRESS>/<SHARE_NAME> /mnt/sia -o username=<USERNAME>,workgroup=<WORKGROUP>,password=<PASSWORD>,uid=$(id -u),gid=$(id -g),rasize=33554432
```
`uid=$(id -u),gid=$(id -g)` mounts the share under the current user's permissions, while `rasize=33554432` increases the buffer size for streaming media files. Both are optional.

4. To unmount, type
```Bash
sudo umount /mnt/sia
```

## Testing

1. Set up a test database

```Bash
sudo -u postgres psql
```
```SQL
CREATE DATABASE sombrero_test;
CREATE USER sombrero_test_user WITH PASSWORD 'sombrero';
ALTER DATABASE sombrero_test OWNER TO sombrero_test_user;
GRANT ALL PRIVILEGES ON DATABASE sombrero_test TO sombrero_test_user;
\q
```
2. Set environment variables
```Bash
export TEST_DB_HOST=127.0.0.1
export TEST_DB_PORT=5432
export TEST_DB_USER=sombrero_test_user
export TEST_DB_PASSWORD=sombrero
export TEST_DB_NAME=sombrero_test
export TEST_DB_SSLMODE=disable
```
3. Run the tests
```Bash
go test ./... -v
```

## Bug Reporting
Please do not hesitate to open an issue if you discover any bugs.

## Acknowledgement
This project was supported by a [Sia Foundation](https://sia.tech) grant.
