#!/usr/bin/env bash
# Shared entrypoint for a restored or already configured local deployment.
set -euo pipefail
umask 077
export PATH="$HOME/.local/bin:$HOME/.merlin-cli/bin:$PATH"
env_file="${MIGRATION_ENV_FILE:-$HOME/.config/machine-migration/env.sh}"
source "$env_file"
state_dir="${CC_CONNECT_DIR:-$HOME/.cc-connect}"
cc_bin="${CC_CONNECT_BIN:-$HOME/.local/bin/cc-connect}"
session=cc-connect-codex
case "${1:-}" in
    --run)
        mkdir -p "$state_dir/logs" "$state_dir/run"
        "$HOME/.local/bin/ensure-codex-proxies"
        exec "$HOME/.local/share/codex-provider/codex_launcher.py" --with-keys \
            "$cc_bin" --config "$state_dir/config.toml" >> "$state_dir/logs/tmux.log" 2>&1
        ;;
    --check)
        test -x "$cc_bin" && test -r "$state_dir/config.toml"
        exec "$HOME/.local/bin/ensure-codex-proxies" --check
        ;;
    '')
        if tmux has-session -t "=$session" 2>/dev/null; then
            echo "cc-connect tmux session already exists: $session"
            exit 0
        fi
        printf -v launch 'exec env MIGRATION_ENV_FILE=%q CC_CONNECT_DIR=%q CC_CONNECT_BIN=%q bash %q --run' \
            "$env_file" "$state_dir" "$cc_bin" "$HOME/.cc-connect/start-codex.sh"
        tmux new-session -d -s "$session" "$launch"
        ;;
    *) echo 'Usage: start-codex.sh [--check|--run]' >&2; exit 2 ;;
esac
