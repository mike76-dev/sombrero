#!/usr/bin/env bash
# Installs Sombrero on Ubuntu as a systemd service, or upgrades an installation it made.
# Run it with sudo; --help lists the options.
set -euo pipefail

REPO=mike76-dev/sombrero
BIN=/usr/local/bin/sombrero
DATA_DIR=/var/lib/sombrero
CONFIG=$DATA_DIR/sombrero.yml
UNIT=/etc/systemd/system/sombrero.service
SERVICE_USER=sombrero
API_ADDRESS=127.0.0.1:9999
DB_NAME=sombrero
DB_USER=sombrero

MODE=""
BINARY=""
RELEASE=""
ASSUME_YES=false
API_PASSWORD=${SOMBRERO_API_PASSWORD:-}
API_PASSWORD_GENERATED=false
FIREWALL_SOURCE=""
DB_PORT=""
DB_PASSWORD=""

usage() {
	cat <<EOF
Usage: sudo bash install-sombrero.sh [options]

Installs Sombrero as a systemd service on Ubuntu, asking for what it needs along the way.
Run again, it upgrades the server and keeps the configuration and the data.

Options:
  --mode normal|lite   the server mode; asked for if omitted
  --version X.Y.Z      the release to install; the latest one if omitted
  --binary PATH        install this binary instead of downloading a release
  --yes                take the defaults instead of asking
  -h, --help           show this help

The API password can be passed in SOMBRERO_API_PASSWORD; otherwise it is asked for or generated.
EOF
}

if [ -t 1 ]; then
	BOLD=$'\e[1m' RED=$'\e[31m' YELLOW=$'\e[33m' GREEN=$'\e[32m' RESET=$'\e[0m'
else
	BOLD="" RED="" YELLOW="" GREEN="" RESET=""
fi

step() { printf '\n%s==> %s%s\n' "$BOLD" "$*" "$RESET"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '%sWarning:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die() {
	printf '%sError:%s %s\n' "$RED" "$RESET" "$*" >&2
	exit 1
}

# The prompts read from the terminal rather than stdin, so that they also work
# when the script itself comes in through a pipe.
need_tty() {
	{ true </dev/tty; } 2>/dev/null || die "there is no terminal to ask on; pass --yes to take the defaults"
}

# ask VAR PROMPT DEFAULT
ask() {
	local answer
	if $ASSUME_YES; then
		printf -v "$1" '%s' "$3"
		return
	fi
	need_tty
	read -r -p "$2 " answer </dev/tty
	printf -v "$1" '%s' "${answer:-$3}"
}

# confirm PROMPT y|n
confirm() {
	local answer hint="[y/N]"
	[ "$2" = y ] && hint="[Y/n]"
	if $ASSUME_YES; then
		[ "$2" = y ]
		return
	fi
	need_tty
	while true; do
		read -r -p "$1 $hint " answer </dev/tty
		case "${answer:-$2}" in
		[Yy]*) return 0 ;;
		[Nn]*) return 1 ;;
		esac
	done
}

# ask_secret VAR PROMPT; an empty answer leaves VAR empty.
ask_secret() {
	local first second
	need_tty
	while true; do
		read -r -s -p "$2 " first </dev/tty
		echo >/dev/tty
		if [ -z "$first" ]; then
			printf -v "$1" ''
			return
		fi
		read -r -s -p "Repeat it: " second </dev/tty
		echo >/dev/tty
		if [ "$first" = "$second" ]; then
			printf -v "$1" '%s' "$first"
			return
		fi
		warn "the two don't match, try again"
	done
}

# od reads a fixed number of bytes, so nothing in the pipe is cut off midway.
random_password() { od -An -N18 -tx1 /dev/urandom | tr -d ' \n'; }

yaml_quote() { printf "'%s'" "${1//\'/\'\'}"; }

# The pipes below read their input to the end: under pipefail, a reader that stops
# early can fail the writer with a broken pipe, and the check with it.
port_in_use() { [ -n "$(ss -ltnH "sport = :$1")" ]; }

APT_UPDATED=false
apt_install() {
	local pkg missing=()
	for pkg in "$@"; do
		dpkg -s "$pkg" >/dev/null 2>&1 || missing+=("$pkg")
	done
	[ ${#missing[@]} -eq 0 ] && return
	info "Installing ${missing[*]}"
	if ! $APT_UPDATED; then
		apt-get update -qq
		APT_UPDATED=true
	fi
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${missing[@]}" >/dev/null
}

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--mode)
			MODE=${2:-}
			shift
			;;
		--version)
			RELEASE=${2:-}
			shift
			;;
		--binary)
			BINARY=${2:-}
			shift
			;;
		--yes) ASSUME_YES=true ;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown option $1; see --help" ;;
		esac
		shift
	done
	case "$MODE" in
	"" | normal | lite) ;;
	*) die "--mode is either normal or lite" ;;
	esac
}

check_system() {
	[ "$(id -u)" -eq 0 ] || die "run it as root: sudo bash $0"
	[ -d /run/systemd/system ] || die "systemd is not running here, and the server is installed as a systemd service"

	local os_id os_name
	os_id=$(. /etc/os-release && echo "${ID:-}")
	os_name=$(. /etc/os-release && echo "${PRETTY_NAME:-unknown}")
	if [ "$os_id" != ubuntu ]; then
		warn "this script is made for Ubuntu, and this is $os_name"
		confirm "Continue anyway?" n || exit 1
	fi

	ARCH=$(dpkg --print-architecture)
	case "$ARCH" in
	amd64 | arm64) ;;
	*) die "there is no Sombrero release for the $ARCH architecture" ;;
	esac
}

# fetch_binary leaves the new binary in $TMP/sombrero and its version in NEW_VERSION.
fetch_binary() {
	if [ -n "$BINARY" ]; then
		[ -f "$BINARY" ] || die "$BINARY does not exist"
		cp "$BINARY" "$TMP/sombrero"
	else
		apt_install curl unzip ca-certificates
		local url=https://github.com/$REPO/releases/latest/download/sombrero_linux_$ARCH.zip
		[ -n "$RELEASE" ] && url=https://github.com/$REPO/releases/download/v$RELEASE/sombrero_linux_$ARCH.zip
		info "Downloading $url"
		curl -fsSL --retry 3 -o "$TMP/sombrero.zip" "$url" || die "couldn't download $url"
		unzip -q -o -j "$TMP/sombrero.zip" sombrero -d "$TMP" || die "the archive holds no sombrero binary"
	fi
	chmod 755 "$TMP/sombrero"
	NEW_VERSION=$("$TMP/sombrero" -version 2>/dev/null) || die "the binary doesn't run on this machine"
}

choose_mode() {
	[ -n "$MODE" ] && return
	cat <<EOF

Sombrero runs in one of two modes:
  1) Normal: renterd and indexd shares, e.g. the Sia Foundation indexer at sia.storage.
     Needs a PostgreSQL database, which this script installs and sets up.
  2) Lite:   renterd shares only, with everything kept in a file instead of a database.
EOF
	local choice
	while true; do
		ask choice "Mode [1]:" 1
		case "$choice" in
		1 | normal) MODE=normal && return ;;
		2 | lite) MODE=lite && return ;;
		esac
	done
}

choose_api_password() {
	[ -n "$API_PASSWORD" ] && return
	if ! $ASSUME_YES; then
		cat <<EOF

The API password protects the web UI and the API, which administer the whole server.
EOF
		ask_secret API_PASSWORD "API password (leave it empty to generate one):"
	fi
	if [ -z "$API_PASSWORD" ]; then
		API_PASSWORD=$(random_password)
		API_PASSWORD_GENERATED=true
	fi
}

# check_ports makes sure the SMB port is free, offering to stop Samba if that is what holds it.
check_ports() {
	if port_in_use 445; then
		if systemctl is-active --quiet smbd 2>/dev/null; then
			cat <<EOF

Samba is running and holds the SMB port 445, which Sombrero needs.
EOF
			confirm "Stop Samba and keep it from starting again?" n || die "port 445 is taken by Samba"
			STOP_SAMBA=true
		else
			ss -ltnpH "sport = :445" >&2
			die "port 445 is taken by the program above; Sombrero needs it"
		fi
	fi
	port_in_use "${API_ADDRESS##*:}" && die "port ${API_ADDRESS##*:}, where the API listens, is taken"
	return 0
}

# choose_firewall sets FIREWALL_SOURCE to what ufw should let reach port 445, or leaves it empty.
choose_firewall() {
	command -v ufw >/dev/null || return 0
	ufw status 2>/dev/null | grep "^Status: active" >/dev/null || return 0

	local iface network="" choice default=3
	iface=$(ip route show default | awk 'NR == 1 {print $5}')
	[ -n "$iface" ] && network=$(ip -o -4 route show dev "$iface" scope link | awk 'NR == 1 {print $1}')
	[ -n "$network" ] && default=1
	cat <<EOF

The ufw firewall is active. Clients reach the server on port 445, which attracts
attackers when it is open to the Internet, so opening it to your local network only is safer.
  1) open it to the local network${network:+ ($network)}
  2) open it to everyone
  3) leave the firewall as it is
EOF
	while true; do
		ask choice "Choice [$default]:" "$default"
		case "$choice" in
		1) [ -n "$network" ] && FIREWALL_SOURCE=$network && return ;;
		2) FIREWALL_SOURCE=any && return ;;
		3) return ;;
		esac
	done
}

# online_cluster prints the version and the port of a running PostgreSQL cluster,
# of the given version if one is named.
online_cluster() {
	pg_lsclusters -h 2>/dev/null |
		awk -v want="${1:-}" '$4 == "online" && (want == "" || $1 == want) && !found {print $1, $3; found = 1}' || true
}

setup_postgres() {
	local cluster
	cluster=$(online_cluster)
	if [ -z "$cluster" ]; then
		info "Installing PostgreSQL 18 from the PostgreSQL repository"
		apt_install postgresql-common
		/usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y >/dev/null
		APT_UPDATED=true
		apt_install postgresql-18
		# The package does not always start the cluster, e.g. where the install is sandboxed.
		systemctl enable --now postgresql >/dev/null 2>&1 || true
		cluster=$(online_cluster 18)
		[ -n "$cluster" ] || cluster=$(online_cluster)
		[ -n "$cluster" ] || die "PostgreSQL was installed, but no cluster of it is running: see pg_lsclusters"
	fi
	local pg_version
	read -r pg_version DB_PORT <<<"$cluster"
	info "Using PostgreSQL $pg_version on port $DB_PORT"

	# The server creates the tables itself; the database only needs a user and a place for them.
	DB_PASSWORD=$(random_password)
	(cd / && runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 -p "$DB_PORT" \
		-v user="$DB_USER" -v db="$DB_NAME" -v pw="$DB_PASSWORD") <<'SQL'
SELECT format('CREATE ROLE %I LOGIN', :'user') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'user') \gexec
ALTER ROLE :"user" WITH LOGIN PASSWORD :'pw';
SELECT format('CREATE DATABASE %I OWNER %I', :'db', :'user') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'db') \gexec
\c :db
GRANT USAGE, CREATE ON SCHEMA public TO :"user";
SQL
	info "Created the database $DB_NAME and its user $DB_USER"
}

create_service_user() {
	id -u "$SERVICE_USER" >/dev/null 2>&1 && return
	useradd --system --home-dir "$DATA_DIR" --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
	info "Created the system user $SERVICE_USER"
}

write_config() {
	install -d -m 700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$DATA_DIR"
	# Written under a restrictive umask, since it holds the passwords.
	(
		umask 077
		{
			cat <<EOF
debug: false
mode: $MODE
maxConnections: 30
api:
  address: $API_ADDRESS
  password: $(yaml_quote "$API_PASSWORD")
EOF
			if [ "$MODE" = normal ]; then
				cat <<EOF
database:
  host: 127.0.0.1
  port: $DB_PORT
  user: $DB_USER
  password: $(yaml_quote "$DB_PASSWORD")
  database: $DB_NAME
  sslMode: disable
indexd:
  appName: Sombrero
  description: Sombrero SMB server
  logoURL: https://raw.githubusercontent.com/mike76-dev/sombrero/master/logo.png
  serviceURL: https://github.com/mike76-dev/sombrero
  seedPhrase: ''
EOF
			fi
		} >"$CONFIG"
	)
	chown "$SERVICE_USER:$SERVICE_USER" "$CONFIG"
	info "Wrote $CONFIG"
}

# allow_bbr lets the server, which doesn't run as root, pick the BBR congestion control it asks for.
allow_bbr() {
	local allowed
	allowed=$(cat /proc/sys/net/ipv4/tcp_allowed_congestion_control 2>/dev/null) || return 0
	case " $allowed " in *" bbr "*) return 0 ;; esac
	if ! modprobe tcp_bbr 2>/dev/null; then
		info "BBR congestion control isn't available; the system default is used instead"
		return 0
	fi
	echo tcp_bbr >/etc/modules-load.d/sombrero.conf
	echo "net.ipv4.tcp_allowed_congestion_control = $allowed bbr" >/etc/sysctl.d/90-sombrero.conf
	sysctl -q -p /etc/sysctl.d/90-sombrero.conf || warn "couldn't allow BBR congestion control"
	info "Allowed the BBR congestion control for the server's connections"
}

write_unit() {
	cat >"$UNIT" <<EOF
[Unit]
Description=Sombrero SMB server
Documentation=https://github.com/$REPO
Wants=network-online.target
After=network-online.target postgresql.service

[Service]
User=$SERVICE_USER
Group=$SERVICE_USER
ExecStart=$BIN --dir=$DATA_DIR
Restart=on-failure
RestartSec=5
# Leaves time for the uploads in flight to drain.
TimeoutStopSec=60
StateDirectory=sombrero
StateDirectoryMode=0700

# Port 445 is the only privilege the server needs.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
EOF
	systemctl daemon-reload
}

install_binary() {
	install -m 755 "$TMP/sombrero" "$BIN"
	info "Installed Sombrero $NEW_VERSION as $BIN"
}

wait_for_api() {
	for _ in $(seq 30); do
		curl -fsS -o /dev/null -u ":$API_PASSWORD" "http://$API_ADDRESS/api/version" 2>/dev/null && return 0
		sleep 1
	done
	journalctl -u sombrero -n 30 --no-pager >&2 || true
	die "the server didn't come up; its log is above"
}

wait_for_service() {
	for _ in $(seq 10); do
		sleep 1
		systemctl is-active --quiet sombrero || break
	done
	systemctl is-active --quiet sombrero && return 0
	journalctl -u sombrero -n 30 --no-pager >&2 || true
	die "the server didn't come up; its log is above"
}

upgrade() {
	local old_version
	old_version=$("$BIN" -version 2>/dev/null) || old_version=unknown
	step "Sombrero is already installed"
	info "Its configuration in $CONFIG and its data are kept."
	fetch_binary
	if [ "$old_version" = "$NEW_VERSION" ] && [ -z "$BINARY" ]; then
		info "Version $NEW_VERSION is installed already, so there is nothing to upgrade."
		exit 0
	fi
	confirm "Upgrade from $old_version to $NEW_VERSION?" y || exit 1

	systemctl stop sombrero 2>/dev/null || true
	install_binary
	write_unit
	systemctl enable --now sombrero >/dev/null 2>&1
	wait_for_service
	step "${GREEN}Sombrero $NEW_VERSION is running${RESET}"
	info "Logs: journalctl -u sombrero -f"
}

install_fresh() {
	step "Installing Sombrero"
	choose_mode
	choose_api_password
	STOP_SAMBA=false
	check_ports
	choose_firewall

	local source="the latest release"
	[ -n "$RELEASE" ] && source="release $RELEASE"
	[ -n "$BINARY" ] && source=$BINARY
	cat <<EOF

${BOLD}The plan:${RESET}
  - install Sombrero from $source as $BIN
EOF
	[ "$MODE" = normal ] && echo "  - set up PostgreSQL, installing it first if it isn't there"
	cat <<EOF
  - run the server in the $MODE mode as the systemd service sombrero, under a user of its own
  - keep its configuration and data in $DATA_DIR
EOF
	$STOP_SAMBA && echo "  - stop and disable Samba"
	[ -n "$FIREWALL_SOURCE" ] && echo "  - open port 445 in ufw to ${FIREWALL_SOURCE/any/everyone}"
	echo
	confirm "Go ahead?" y || exit 1

	step "Getting Sombrero"
	fetch_binary
	info "Got version $NEW_VERSION"
	apt_install curl

	if [ "$MODE" = normal ]; then
		step "Setting up PostgreSQL"
		setup_postgres
	fi

	step "Installing the service"
	if $STOP_SAMBA; then
		systemctl disable --now smbd >/dev/null 2>&1
		info "Stopped and disabled Samba"
	fi
	create_service_user
	install_binary
	write_config
	allow_bbr
	write_unit
	if [ -n "$FIREWALL_SOURCE" ]; then
		if [ "$FIREWALL_SOURCE" = any ]; then
			ufw allow 445/tcp comment Sombrero >/dev/null
		else
			ufw allow from "$FIREWALL_SOURCE" to any port 445 proto tcp comment Sombrero >/dev/null
		fi
		info "Opened port 445 in ufw"
	fi

	step "Starting the server"
	systemctl enable --now sombrero >/dev/null 2>&1
	wait_for_api
	info "The server is up"
	summary
}

summary() {
	local address
	address=$(hostname -I 2>/dev/null | awk '{print $1}')
	step "${GREEN}Sombrero $NEW_VERSION is installed and running${RESET}"
	cat <<EOF

  Web UI and API:  http://$API_ADDRESS (reachable from this machine only)
EOF
	if $API_PASSWORD_GENERATED; then
		echo "  API password:    $API_PASSWORD"
	else
		echo "  API password:    the one you entered"
	fi
	cat <<EOF
  Configuration:   $CONFIG
  Logs:            journalctl -u sombrero -f
  Service:         systemctl status|restart|stop sombrero

${BOLD}What's next${RESET}
  1. Open the web UI. From another computer, go through an SSH tunnel:
       ssh -N -L 9999:$API_ADDRESS <user>@${address:-<this server>}
     and then open http://127.0.0.1:9999 in the browser there.
  2. Enter the API password on the Settings page.
  3. Open the Setup wizard, which walks you through creating a workgroup and an
     account, registering a share, connecting to it, and granting the account access.
  4. Connect to \\\\${address:-<this server>}\\<share> from your computers with that account.
EOF
	if [ "$MODE" = normal ]; then
		cat <<EOF

The server generated a seed phrase for indexd shares and saved it in the configuration.
Back it up somewhere safe, since the data on indexd shares can't be recovered without it:
  sudo grep seedPhrase $CONFIG
EOF
	fi
	if $API_PASSWORD_GENERATED; then
		echo
		echo "The API password is also in the configuration, should you need it again."
	fi
}

main() {
	parse_args "$@"
	check_system
	TMP=$(mktemp -d)
	trap 'rm -rf "$TMP"' EXIT

	if [ -f "$CONFIG" ]; then
		upgrade
	else
		install_fresh
	fi
}

main "$@"
