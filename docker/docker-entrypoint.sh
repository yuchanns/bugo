#!/usr/bin/env bash
set -euo pipefail

if [[ "${BUGO_ENABLE_XVFB:-1}" == "1" ]]; then
    export DISPLAY="${DISPLAY:-:99}"
    extra_xvfb_args=()
    if [[ -n "${XVFB_ARGS:-}" ]]; then
        read -r -a extra_xvfb_args <<<"${XVFB_ARGS}"
    fi

    Xvfb "${DISPLAY}" \
        -screen 0 "${XVFB_RESOLUTION:-1920x1080x24}" \
        -nolisten tcp \
        -ac \
        +extension RANDR \
        "${extra_xvfb_args[@]}" \
        >/tmp/xvfb.log 2>&1 &

    for _ in $(seq 1 50); do
        if xdpyinfo -display "${DISPLAY}" >/dev/null 2>&1; then
            break
        fi
        sleep 0.1
    done

    if ! xdpyinfo -display "${DISPLAY}" >/dev/null 2>&1; then
        echo "failed to start Xvfb on ${DISPLAY}; see /tmp/xvfb.log" >&2
        exit 1
    fi
fi

exec "$@"
