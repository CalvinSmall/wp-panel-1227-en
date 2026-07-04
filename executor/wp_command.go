package executor

import (
	"log"
	"os"
)

const wpScript = `#!/bin/bash
# WP Panel CLI — wp

BIN=/usr/local/bin/wp-panel
CFG=/www/server/panel/config.json
SVC=wp-panel

red()  { echo -e "\033[31m$*\033[0m"; }
green(){ echo -e "\033[32m$*\033[0m"; }
dim()  { echo -e "\033[2m$*\033[0m"; }

diag() {
    local issues=0

    # 1. Binary
    if [ -x "$BIN" ]; then
        green "✓ Binary: $BIN"
    elif [ -f "$BIN" ]; then
        red "✗ Binary is not executable: $BIN"
        echo "   → Fix: chmod +x $BIN"
        issues=$((issues+1))
    else
        red "✗ Binary not found: $BIN"
        echo "   → Panel may not be installed or installation is incomplete. Please run install.sh again"
        issues=$((issues+1))
        return $issues
    fi

    # 2. Config file
    if [ -f "$CFG" ]; then
        if python3 -c "import json; json.load(open('$CFG'))" 2>/dev/null; then
            green "✓ Config file: $CFG"
        else
            red "✗ Config file has invalid JSON: $CFG"
            echo "   → Fix: check file contents or restore from backup"
            issues=$((issues+1))
        fi
    else
        red "✗ Config file not found: $CFG"
        echo "   → Panel may not be installed. Please run install.sh again"
        issues=$((issues+1))
        return $issues
    fi

    # 3. Database
    DB=$(python3 -c "import json; d=json.load(open('$CFG')); print(d.get('sqlite',{}).get('path',''))" 2>/dev/null)
    if [ -n "$DB" ] && [ -f "$DB" ]; then
        green "✓ Database: $DB"
    elif [ -n "$DB" ]; then
        red "✗ Database file not found: $DB"
        echo "   → Database file is missing. Check disk space or restore from backup"
        issues=$((issues+1))
    else
        dim "? Could not read database path"
    fi

    # 4. systemd service file
    if [ -f "/etc/systemd/system/${SVC}.service" ]; then
        green "✓ systemd service file: /etc/systemd/system/${SVC}.service"
    else
        red "✗ systemd service file missing"
        echo "   → Fix: run install.sh again"
        issues=$((issues+1))
        return $issues
    fi

    # 5. Port
    PORT=$(python3 -c "import json; d=json.load(open('$CFG')); print(d['panel'].get('tls_port', d['panel']['port']))" 2>/dev/null)
    if [ -n "$PORT" ]; then
        if ss -tlnp 2>/dev/null | grep -q ":${PORT} "; then
            green "✓ Port ${PORT} is listening"
        else
            dim "? Port ${PORT} is not listening (panel is not running)"
        fi
    fi

    # 6. systemd status
    if systemctl is-active --quiet "$SVC"; then
        green "✓ Service status: running"
    else
        red "✗ Service status: not running"
        issues=$((issues+1))
        echo ""
        echo "── Recent error logs ──"
        journalctl -u "$SVC" -n 20 --no-pager --lines=6 2>/dev/null | tail -20
        echo "── End of logs ──"
        echo ""
        echo "→ View full logs: journalctl -u $SVC -n 50 --no-pager"
    fi

    if [ $issues -eq 0 ]; then
        echo ""
        green "All checks passed"
    else
        echo ""
        red "Found ${issues} issue(s)"
    fi
}

case "${1:-}" in
    restart)
        echo "Restarting panel..."
        if systemctl restart "$SVC" 2>/dev/null; then
            sleep 2
            if systemctl is-active --quiet "$SVC"; then
                green "WP Panel restarted and running"
            else
                red "WP Panel failed to start after restart"
                echo ""
                echo "── Recent logs ──"
                journalctl -u "$SVC" -n 20 --no-pager 2>/dev/null | tail -20
                echo "── End ──"
                echo ""
                echo "→ Run 'wp status' for full diagnostics"
            fi
        else
            red "systemctl restart failed. Service may not be installed"
            echo "→ Run 'wp status' for diagnostics"
        fi
        ;;
    password)
        $BIN --reset-admin
        ;;
    info)
        $BIN --info
        ;;
    unban)
        $BIN --unban-all
        ;;
    status|check)
        echo "WP Panel Diagnostic Check"
        echo "========================="
        echo ""
        diag
        ;;
    log)
        journalctl -u "$SVC" -n "${2:-30}" --no-pager 2>/dev/null
        ;;
    *)
        $BIN --info 2>/dev/null
        echo ""
        if [ -f "$CFG" ]; then
            PORT=$(python3 -c "import json; d=json.load(open('$CFG')); print(d['panel']['port'])" 2>/dev/null)
            SUFFIX=$(python3 -c "import json; d=json.load(open('$CFG')); print(d['panel']['random_suffix'])" 2>/dev/null)
            IP=$(hostname -I 2>/dev/null | awk '{print $1}')
            TLS_PORT=$(python3 -c "import json; d=json.load(open('$CFG')); print(d['panel'].get('tls_port', d['panel']['port']))" 2>/dev/null)
            [ -n "$TLS_PORT" ] && [ -n "$SUFFIX" ] && [ -n "$IP" ] && echo "Panel address: https://$IP:$TLS_PORT/$SUFFIX"
        fi
        if systemctl is-active --quiet "$SVC"; then
            green "Running status: running"
        else
            red "Running status: not running"
            echo ""
            echo "── Auto diagnostics ──"
            diag
        fi
        echo ""
        echo "Usage: wp <command>"
        echo "  wp restart     Restart the panel"
        echo "  wp status      Full diagnostic check"
        echo "  wp log [N]     Show last N log entries (default: 30)"
        echo "  wp password    Reset admin account password"
        echo "  wp unban       Clear all IP bans"
        ;;
esac
`

func EnsureWPCommand() {
	path := "/usr/local/bin/wp"
	if err := os.WriteFile(path, []byte(wpScript), 0755); err != nil {
		log.Printf("Failed to install wp command (%s): %v", path, err)
	}
}
