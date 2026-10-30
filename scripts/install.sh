#!/usr/bin/env bash
set -euo pipefail

BINARY_NAME="rook"
SERVICE_NAME="rook"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/rook"
CONFIG_FILE="${CONFIG_DIR}/rook.yaml"
STATE_DIR="/var/lib/rook"
DOWNLOAD_BASE="${ROOK_DOWNLOAD_BASE:-https://github.com/phpsandbox/rook/releases}"

usage() {
  cat <<EOF
Usage: install.sh [OPTIONS]

Install or update Rook and configure it as a systemd service.

Options:
  --server-id ID       Server ID for this agent (required for fresh install)
  --token TOKEN        Authentication token (required for fresh install)
  --control-plane URL  Control plane websocket URL (required for fresh install)
  --version VERSION    Install a specific version, for example v0.1.0 (default: latest)
  --managed-updates    Allow owner-requested updates (automatic updates stay disabled)
  --auto-update        Opt into automatic updates within the installed minor version
  --uninstall          Remove Rook and its service
  --purge              With --uninstall, also remove configuration, state, and the rook user
  -h, --help           Show this help

Environment:
  ROOK_DOWNLOAD_BASE   Override release download base URL
EOF
  exit 0
}

log() { printf '\033[1;34m=>\033[0m %s\n' "$*"; }
err() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

SERVER_ID=""
TOKEN=""
CONTROL_PLANE=""
VERSION="latest"
UNINSTALL=false
PURGE=false
MANAGED_UPDATES=false
AUTO_UPDATE=false
AUTO_UPDATE_SUPPORTED=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --server-id) SERVER_ID="${2:-}"; shift 2 ;;
    --token) TOKEN="${2:-}"; shift 2 ;;
    --control-plane) CONTROL_PLANE="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --managed-updates) MANAGED_UPDATES=true; shift ;;
    --auto-update) AUTO_UPDATE=true; shift ;;
    --uninstall) UNINSTALL=true; shift ;;
    --purge) PURGE=true; shift ;;
    -h|--help) usage ;;
    *) err "unknown option: $1" ;;
  esac
done

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) err "unsupported architecture: $(uname -m)" ;;
  esac
}

detect_os() {
  case "$(uname -s | tr '[:upper:]' '[:lower:]')" in
    linux) echo "linux" ;;
    *) err "unsupported OS: $(uname -s). Rook currently supports Linux." ;;
  esac
}

check_root() {
  if [[ "${EUID}" -ne 0 ]]; then
    err "this script must be run as root; use sudo"
  fi
}

check_docker() {
  command -v git >/dev/null 2>&1 || err "git is required for publishing"
  command -v curl >/dev/null 2>&1 || err "curl is required for publishing health checks"
  if ! command -v docker >/dev/null 2>&1; then
    err "docker is not installed"
  fi
  if ! docker info >/dev/null 2>&1; then
    err "docker daemon is not running or is not accessible"
  fi
  if ! docker compose version >/dev/null 2>&1; then
    err "the Docker Compose plugin is required"
  fi
  local compose_help
  compose_help="$(docker compose up --help)" || err "cannot inspect Docker Compose up support"
  if ! grep -Eq -- '(^|[[:space:],])--wait([[:space:],]|$)' <<<"$compose_help" || ! grep -Fq -- '--wait-timeout' <<<"$compose_help"; then
    err "Docker Compose must support up --wait and --wait-timeout; update the Compose plugin"
  fi
  compose_help="$(docker compose down --help)" || err "cannot inspect Docker Compose down support"
  if ! grep -Fq -- '--remove-orphans' <<<"$compose_help"; then
    err "Docker Compose must support down --remove-orphans"
  fi
}

download() {
  local url="$1"
  local output="$2"

  if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$output" "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$output" "$url"
  else
    err "neither curl nor wget is installed"
  fi
}

asset_url() {
  local version="$1"
  local asset="$2"

  if [[ "$version" == "latest" ]]; then
    printf '%s/latest/download/%s' "$DOWNLOAD_BASE" "$asset"
  else
    printf '%s/download/%s/%s' "$DOWNLOAD_BASE" "$version" "$asset"
  fi
}

verify_checksum() {
  local binary_path="$1"
  local checksum_path="$2"
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$(dirname "$binary_path")" && sha256sum -c "$(basename "$checksum_path")")
  elif command -v shasum >/dev/null 2>&1; then
    (cd "$(dirname "$binary_path")" && shasum -a 256 -c "$(basename "$checksum_path")")
  else
    err "neither sha256sum nor shasum is installed"
  fi
}

create_user() {
  if id rook >/dev/null 2>&1; then
    log "User rook already exists"
  else
    useradd --system --home-dir "$STATE_DIR" --shell /usr/sbin/nologin --create-home rook
    log "Created system user rook"
  fi

  if groups rook | grep -q '\bdocker\b'; then
    log "User rook already belongs to docker group"
  else
    usermod -aG docker rook
    log "Added rook to docker group"
  fi
}

download_binary() {
  local os arch asset tmpdir binary_tmp checksum_tmp
  os="$(detect_os)"
  arch="$(detect_arch)"
  asset="${BINARY_NAME}-${os}-${arch}"
  tmpdir="$(mktemp -d)"
  binary_tmp="${tmpdir}/${asset}"
  checksum_tmp="${tmpdir}/${asset}.sha256"

  log "Downloading ${asset} (${VERSION})"
  download "$(asset_url "$VERSION" "$asset")" "$binary_tmp"
  download "$(asset_url "$VERSION" "${asset}.sha256")" "$checksum_tmp"
  verify_checksum "$binary_tmp" "$checksum_tmp"
  chmod 0700 "$binary_tmp"
  if "$binary_tmp" --help 2>&1 | grep -q -- '^  -auto-update'; then
    AUTO_UPDATE_SUPPORTED=true
  elif [[ "$AUTO_UPDATE" == "true" ]]; then
    err "selected Rook version does not support --auto-update"
  fi

  if [[ "$MANAGED_UPDATES" == "true" ]] && ! "$binary_tmp" --help 2>&1 | grep -q -- '^  -managed-updates'; then
    err "selected Rook version does not support --managed-updates"
  fi

  local staged
  staged="$(mktemp "${INSTALL_DIR}/.rook-install.XXXXXX")"
  install -m 0755 "$binary_tmp" "$staged"
  mv -f "$staged" "${INSTALL_DIR}/${BINARY_NAME}"
  # A manual install supersedes any interrupted automatic update.
  rm -f "${INSTALL_DIR}/.rook-update/pending.json" "${INSTALL_DIR}/.rook-update/failed-versions"
  rm -rf "$tmpdir"
  log "Installed ${INSTALL_DIR}/${BINARY_NAME}"
}

write_config() {
  install -d -m 0750 -o root -g rook "$CONFIG_DIR"
  cat > "$CONFIG_FILE" <<YAML
server_id: "${SERVER_ID}"
token: "${TOKEN}"
control_plane: "${CONTROL_PLANE}"
state_dir: "${STATE_DIR}/state"
YAML
  chown root:rook "$CONFIG_FILE"
  chmod 0640 "$CONFIG_FILE"
  log "Wrote ${CONFIG_FILE}"
}

write_systemd_unit() {
  local auto_update_arg=""
  if [[ "$MANAGED_UPDATES" == "true" ]]; then
    auto_update_arg=" --managed-updates"
  fi
  if [[ "$AUTO_UPDATE" == "true" ]]; then
    auto_update_arg="${auto_update_arg} --auto-update"
  fi
  cat > "/etc/systemd/system/${SERVICE_NAME}.service" <<UNIT
[Unit]
Description=PHPSandbox Rook
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
User=rook
Group=rook
WorkingDirectory=${STATE_DIR}
RuntimeDirectory=rook
RuntimeDirectoryMode=0700
Environment=HOME=${STATE_DIR}
ExecStart=${INSTALL_DIR}/${BINARY_NAME} --config ${CONFIG_FILE}${auto_update_arg}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT

  if [[ "$AUTO_UPDATE_SUPPORTED" == "true" ]]; then
    cat > "/etc/systemd/system/rook-update.service" <<UNIT
[Unit]
Description=Verify and apply opted-in Rook patch updates
After=network-online.target rook.service
Wants=network-online.target

[Service]
Type=oneshot
User=root
Environment=PATH=/usr/local/bin:/usr/bin:/bin
ExecStart=${INSTALL_DIR}/${BINARY_NAME} --apply-auto-update
TimeoutStartSec=10min
TimeoutStopSec=4min
UMask=0077
UNIT

    cat > "/etc/systemd/system/rook-update.timer" <<UNIT
[Unit]
Description=Check opted-in Rook agents for patch updates daily

[Timer]
OnBootSec=1min
OnUnitInactiveSec=1min
RandomizedDelaySec=15s
Unit=rook-update.service

[Install]
WantedBy=timers.target
UNIT
  else
    systemctl stop rook-update.timer rook-update.service 2>/dev/null || true
    systemctl disable rook-update.timer 2>/dev/null || true
    rm -f /etc/systemd/system/rook-update.service /etc/systemd/system/rook-update.timer
  fi

  systemctl daemon-reload
  log "Wrote systemd unit"
}

start_service() {
  systemctl enable "$SERVICE_NAME"
  systemctl restart "$SERVICE_NAME"
  # The helper makes no network requests unless the daemon exposes its opt-in
  # socket. Keeping the timer installed lets ExecStart control the feature.
  if [[ "$AUTO_UPDATE_SUPPORTED" == "true" ]]; then
    systemctl enable --now rook-update.timer
  fi
  log "Started ${SERVICE_NAME}.service"
}

uninstall() {
  systemctl stop rook-update.timer rook-update.service 2>/dev/null || true
  systemctl disable rook-update.timer 2>/dev/null || true
  systemctl stop "$SERVICE_NAME" 2>/dev/null || true
  systemctl disable "$SERVICE_NAME" 2>/dev/null || true
  rm -f "${INSTALL_DIR}/${BINARY_NAME}"
  rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
  rm -f /etc/systemd/system/rook-update.service /etc/systemd/system/rook-update.timer
  rm -rf "${INSTALL_DIR}/.rook-update"
  systemctl daemon-reload
  if [[ "$PURGE" == "true" ]]; then
    rm -rf "$CONFIG_DIR" "$STATE_DIR"
    if id rook >/dev/null 2>&1; then
      userdel rook
    fi
    log "Rook, its credentials, and local state were removed."
  else
    log "Rook removed. Config and state were preserved at ${CONFIG_DIR} and ${STATE_DIR}."
  fi
}

main() {
  check_root

  if [[ "$PURGE" == "true" && "$UNINSTALL" != "true" ]]; then
    err "--purge requires --uninstall"
  fi

  if [[ "$UNINSTALL" == "true" ]]; then
    uninstall
    return
  fi

  check_docker
  create_user

  local fresh_install=true
  if [[ -f "$CONFIG_FILE" ]]; then
    fresh_install=false
  fi
  if [[ "$fresh_install" == "true" && (-z "$SERVER_ID" || -z "$TOKEN" || -z "$CONTROL_PLANE") ]]; then
    err "fresh install requires --server-id, --token, and --control-plane"
  fi

  install -d -m 0755 "$INSTALL_DIR"
  # Stop the managed updater before replacing its executable. Preserve the
  # existing timer if installation fails, without requiring another host tool.
  if systemctl is-active --quiet rook-update.timer; then
    trap 'systemctl start rook-update.timer >/dev/null 2>&1 || true' EXIT
  fi
  if ! systemctl stop rook-update.timer rook-update.service 2>/dev/null; then
    if systemctl is-active --quiet rook-update.service || systemctl is-active --quiet rook-update.timer; then
      err "could not stop automatic updates before installation"
    fi
  fi
  install -d -m 0700 -o root -g root "${INSTALL_DIR}/.rook-update"
  install -d -m 0750 -o rook -g rook "$STATE_DIR" "${STATE_DIR}/state"
  download_binary

  if [[ "$fresh_install" == "true" ]]; then
    write_config
  else
    log "Keeping existing ${CONFIG_FILE}"
  fi

  write_systemd_unit
  start_service

  local installed_version
  installed_version="$("${INSTALL_DIR}/${BINARY_NAME}" --version 2>/dev/null || echo "unknown")"
  log "Rook installed: ${installed_version}"
}

main "$@"
