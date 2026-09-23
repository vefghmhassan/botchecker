#!/bin/sh
# Start botchecker with the environment from .env.
#
# The secrets stay in .env and are only ever loaded into this process. Logs go
# to stdout; redirect them if you want to keep them:
#   ./run.sh > botchecker.log 2>&1 &
#
# Anything already set in the environment wins over the file, which is the usual
# dotenv rule and the reason DB_PATH can be pointed at the local data directory
# without editing a file whose value is right for the container:
#   DB_PATH=data/botchecker.db ./run.sh
set -e
cd "$(dirname "$0")"

if [ ! -f .env ]; then
    echo "no .env here — copy .env.example and fill it in" >&2
    exit 1
fi

# Remember what the caller set, so the file cannot overwrite a deliberate
# override. Only the handful of paths and ports worth overriding by hand.
for var in DB_PATH APP_PORT DASHBOARD_USER DASHBOARD_PASS LOG_LEVEL; do
    eval "value=\${$var-}"
    if [ -n "$value" ]; then
        eval "__override_$var=\$value"
    fi
done

set -a
. ./.env
set +a

for var in DB_PATH APP_PORT DASHBOARD_USER DASHBOARD_PASS LOG_LEVEL; do
    eval "saved=\${__override_$var-}"
    if [ -n "$saved" ]; then
        eval "export $var=\$saved"
    fi
done

# Refuse to start a second copy: two instances on one bot token fight over
# getUpdates, and the taps land on whichever happens to poll first.
if pid=$(lsof -nP -iTCP:"${APP_PORT:-8081}" -sTCP:LISTEN -t 2>/dev/null); then
    echo "something is already listening on port ${APP_PORT:-8081} (pid $pid)" >&2
    echo "stop it first: kill $pid" >&2
    exit 1
fi

echo "starting botchecker: port=${APP_PORT:-8081} db=${DB_PATH:-data/botchecker.db}" >&2
exec ./botchecker
