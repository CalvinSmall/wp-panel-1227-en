#!/bin/bash
set -e
set -o pipefail

# ============================================================
# WP Panel China Entry Script
# Main installation logic is maintained in install.sh.
# Here we only enable China-priority strategy and fetch the main script from multiple fixed sources during pipeline installation.
# ============================================================

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

GITHUB_INSTALL_URL="https://raw.githubusercontent.com/CalvinSmall/wp-panel-1227-en/main/install.sh"
INSTALL_SCRIPT_SOURCES=(
    "gh.wp-panel.org reverse proxy|https://gh.wp-panel.org/${GITHUB_INSTALL_URL}"
    "jsDelivr reverse proxy|https://cdn.jsdelivr.net/gh/CalvinSmall/wp-panel-1227-en@main/install.sh"
    "GitHub direct|${GITHUB_INSTALL_URL}"
)

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

export WP_PANEL_PREFER_CN_MIRROR=1

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || true)"
if [[ -n "$SCRIPT_DIR" ]] && [[ -f "$SCRIPT_DIR/install.sh" ]]; then
    log_info "Using install.sh from same directory, enabling China-priority strategy"
    exec bash "$SCRIPT_DIR/install.sh" --prefer-cn "$@"
fi

download_install_script() {
    local url="$1"
    if command -v wget &>/dev/null; then
        wget -qO- "$url" 2>/dev/null && return 0
    fi
    if command -v curl &>/dev/null; then
        curl -fsSL "$url" 2>/dev/null && return 0
    fi
    return 1
}

validate_install_script() {
    local content="$1"
    [[ "$content" == *"WP Panel install script"* ]] && [[ "$content" == *"Deploy panel binary"* ]]
}

INSTALL_SCRIPT=""
for source in "${INSTALL_SCRIPT_SOURCES[@]}"; do
    label="${source%%|*}"
    url="${source#*|}"

    log_info "Fetching main install script via ${label}..."
    CANDIDATE_SCRIPT="$(download_install_script "$url" || true)"
    if [[ -n "$CANDIDATE_SCRIPT" ]] && validate_install_script "$CANDIDATE_SCRIPT"; then
        INSTALL_SCRIPT="$CANDIDATE_SCRIPT"
        log_info "Main install script fetched successfully: ${label}"
        break
    fi
    log_warn "${label} fetch failed or content abnormal, trying next source..."
done

if [[ -z "$INSTALL_SCRIPT" ]]; then
    log_error "Cannot obtain install.sh. Suggested solutions:
  1. Check if the server can access gh.wp-panel.org, cdn.jsdelivr.net, or GitHub
  2. Manually download install.sh and run: bash install.sh --prefer-cn
  3. If GitHub Releases is also inaccessible, also download the release attachment wp-panel, place it in the same directory as install.sh, and re-run"
fi

exec bash -s -- --prefer-cn "$@" <<< "$INSTALL_SCRIPT"
