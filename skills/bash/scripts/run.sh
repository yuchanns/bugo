#!/usr/bin/env bash
set -euo pipefail

: "${BUGO_WORKSPACE:?BUGO_WORKSPACE is required}"
: "${BUGO_CMD:?BUGO_CMD is required}"

cd "${BUGO_WORKSPACE}"
exec bash --noprofile --norc -lc "${BUGO_CMD}"
