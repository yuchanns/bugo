#!/usr/bin/env bash
set -euo pipefail

: "${BUGO_WORKSPACE:?BUGO_WORKSPACE is required}"
: "${BUGO_CMD:?BUGO_CMD is required}"

rtk_config_path() {
    case "$(uname -s)" in
        Darwin*)
            printf '%s\n' "${HOME}/Library/Application Support/rtk/config.toml"
            ;;
        *)
            if [[ -n "${XDG_CONFIG_HOME:-}" ]]; then
                printf '%s\n' "${XDG_CONFIG_HOME}/rtk/config.toml"
            else
                printf '%s\n' "${HOME}/.config/rtk/config.toml"
            fi
            ;;
    esac
}

cmd="${BUGO_CMD}"
if command -v rtk >/dev/null 2>&1 && [[ -f "$(rtk_config_path)" ]]; then
    if rewritten="$(rtk rewrite "${cmd}" 2>/dev/null)"; then
        if [[ -n "${rewritten}" ]]; then
            cmd="${rewritten}"
        fi
    fi
fi

cd "${BUGO_WORKSPACE}"
exec bash --noprofile --norc -lc "${cmd}"
