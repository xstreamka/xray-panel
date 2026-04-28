#!/bin/sh
set -eu

CONFIG_PATH="${MTPROTO_CONFIG_PATH:-/config/config.py}"
CONFIG_DIR="$(dirname "${CONFIG_PATH}")"

echo "Waiting for MTProto config: ${CONFIG_PATH}"
while [ ! -s "${CONFIG_PATH}" ]; do
  sleep 1
done

cd "${CONFIG_DIR}"
export PYTHONPATH="${CONFIG_DIR}${PYTHONPATH:+:${PYTHONPATH}}"

exec python3 /opt/mtprotoproxy/mtprotoproxy.py
