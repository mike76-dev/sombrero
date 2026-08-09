<img src="logo.png" width="128">

# Sombrero
This is an SMB server integrated into the Sia decentralized cloud storage. Users can connect to it from their PCs and access the Sia storage like they would normally do with a regular remote drive.

## Prerequisites
* At least one `renterd` or `indexd` node running either locally or on a remote machine is required. The node needs to be funded, have the minimal required number of active storage contracts, and be accessible from the machine where the server is running.
Even though it is possible to use a single or multiple remote `renterd` nodes, it is recommended to run the node locally, to avoid an overhead caused by the additional Internet traffic.
How to set up a `renterd` node is described here: [https://github.com/SiaFoundation/renterd](https://github.com/SiaFoundation/renterd).
The setup process of an `indexd` node is described here: [https://github.com/SiaFoundation/indexd](https://github.com/SiaFoundation/indexd). Alternatively, one can connect to the [Sia Foundation indexer](https://sia.storage/).
* The SMB port 445 needs to be open on the machine where the server is running.

## Limitations
* Guest or anonymous access is not supported.

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
CREATE USER <USER> WITH ENCRYPTED PASSWORD <DB_PASSWORD>;
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
Now we need to create the tables. Open the PostgreSQL shell under the newly created user:
```Bash
psql -U <USER> -d <DATABASE> -h localhost
```
Enter `<DB_PASSWORD>` when prompted to.
Inside the `psql` prompt:
```SQL
\i <PATH_TO_INIT.SQL>
```

## Running the Server
A config file, `sombrero.yml`, needs to be created in the directory where the server will be running. It should contain the following lines:
```YAML
debug: false               # indicates whether to display the session ID and key for tools like Wireshark to decrypt the encrypted data
mode: normal               # the server mode: 'normal' or 'lite' (see below)
maxConnections: 30         # the maximum number of connections accepted from the same IP within 10 minutes
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
{"url":"https://sia.storage/auth/connect/d10f2a960d7dfc947248f58758619b74"}
```
After visiting the URL provided and accepting the connection, run
```Bash
curl -u "":<API_PASSWORD> -X PUT "http://127.0.0.1:9999/api/connect/8303eeb8-f30e-4607-9eb7-875df2c5bd52/shared-indexd"
```
Example of the output:
```Bash
{"appKey":"03a2aab52b79f674354af35b0030cd0cd45b51f53a1a75795a58c85844767b3d3ac38242c05637cac5b8b7fbcea55d29826845fdfc0ef19894d7640438f43a22"}
```
### 5. Grant access to the share
To grant an account access to the share, run:
```Bash
curl -u "":<API_PASSWORD> -X PUT "http://127.0.0.1:9999/api/share/shared-indexd/policy?username=test&workgroup=8303eeb8-f30e-4607-9eb7-875df2c5bd52&read=true&write=true&delete=true&execute=true"
```
## Web UI
The server ships with a web UI covering the same ground as the API: workgroups, accounts, shares, access policies, bans, and the server statistics. It is built into the binary and served at the API address, so there is nothing separate to run or deploy. Open `http://127.0.0.1:9999` in a browser and enter `<API_PASSWORD>` when it asks.

The UI is served without a password, while everything under `/api` still requires one. This is deliberate: the UI holds nothing secret, and asking for the password itself makes for a better login than the browser's basic auth prompt.

### Building the UI
The UI is not built as part of `go build`. Release binaries are built by building the UI first and then the server:
```Bash
npm --prefix web install
npm --prefix web run build
go build .
```
A server built without this step runs normally and serves the API as usual; only the UI is missing, and it says so if you open it in a browser.

To work on the UI itself, run `npm --prefix web run dev` alongside the server. The dev server proxies `/api` through to the server on port 9999, so the URLs match what the embedded build sees. Point the proxy elsewhere with `SOMBRERO_API_URL`.

## Upload Packing
A file whose size is not a multiple of the slab size leaves a piece of data behind that is too small for a slab of its own. Such pieces are kept in the database until they can be packed together into a full slab, which is uploaded as one. By default they are kept for as long as that takes, because an incomplete slab occupies as much storage as a full one. Both config fields are optional: setting `maxBufferAge` (for example, `24h`) uploads them anyway once they have waited that long, while `minPackedSlabSize` (for example, `1048576`) holds that upload back until the leftover data of a share is worth a slab. On its own, `minPackedSlabSize` has no effect.
## Shared Folders
It is now possible to define a list of shared folder names for each workgroup. Files uploaded or moved to such folders are not only visible for those users who uploaded or moved them, but for all members of the workgroup. Only working on `indexd` shares.

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

## Lite Mode
If you only intend to connect to `renterd` shares, you can run the server in the Lite mode by setting `mode: lite` in the config file. In this mode, no PostgreSQL database is required: the shares, workgroups, accounts, access policies, and the ban list are kept in a JSON file (`store.json`) in the data directory, and the `database` and `indexd` sections of the config file may be omitted. `indexd` shares are not supported in the Lite mode.

## Security Considerations
An open TCP port 445 attracts thousands of attackers and those who look for a free storage. For this reason, guest and anonymous accesses are disabled. Even when the server is running on a private LAN, it should not be a problem to create a password-protected account like described above.

The API administers the whole server, so it listens on `127.0.0.1` unless the config file says otherwise, and the server refuses to start without an API password. Repeated failed logins from the same host are throttled. If you do need to reach the API or the web UI from another machine, prefer an SSH tunnel:
```Bash
ssh -N -L 9999:127.0.0.1:9999 <USER>@<SERVER_NET_ADDRESS>
```
Binding the API to a public interface exposes the password over plain HTTP; put a reverse proxy with TLS in front of it if you go that way.

The server also has a built-in abuse protection. If 30 or more connections are detected from the same IP address within 10 minutes, this IP is permanently banned. This number of 30 can be configured in the config file (see above).

Also banned are those remote hosts, which continue sending SMB1 requests after receiving the initial SMB2 response from the server.

The bans are saved in the database, and the reason for the ban is provided. If a host ends up banned by mistake, it can be removed manually:
```Bash
curl -u "":<API_PASSWORD> -X DELETE "http://127.0.0.1:9999/api/bans/<IP_OF_THE_REMOTE_HOST>"
```

## Connecting to the Server
In the guides below, `<SERVER_NET_ADDRESS>` stands for the network address of the SMB server, while `<SHARE_NAME>` is the name of the share registered earlier.

### Windows
1. Right-click on `This PC` icon and choose `Map network drive...` from the popup menu.
2. Type the address of the share in the `Folder` field (`\\<SERVER_NET_ADDRESS>\<SHARE_NAME>`). Pick any drive letter. Check the `Connect using different credentials` box, then click `Finish`.
3. In the next popup window, enter the user credentials (matching one of the registered accounts) and click `OK`.

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
sudo mount -t cifs //<SERVER_NET_ADDRESS>/<SHARE_NAME> /mnt/sia -o username=<USERNAME>,workgroup=<WORKGROUP>,password=<PASSWORD>
```
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
export TEST_INIT_SQL=./init.sql
```
3. Run the tests
```Bash
go test ./... -v
```

## Bug Reporting
Please do not hesitate to open an issue if you discover any bugs.

## Acknowledgement
This project was supported by a [Sia Foundation](https://sia.tech) grant.
