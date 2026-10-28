#!/bin/sh
# Throwaway Postgres for development and tests, kept in ./.pgdata.
# It is not a system service: `start` also reloads the schema and seed data,
# so every run begins from the same state. `stop` shuts it down.
set -eu
cd "$(dirname "$0")/.."

DATA=.pgdata
PORT=${PGPORT:-54329}
PSQL="psql -q -v ON_ERROR_STOP=1 -h localhost -p $PORT -U triage"

case "${1:-start}" in
start)
    if [ ! -d "$DATA" ]; then
        initdb -D "$DATA" -U triage --auth=trust >/dev/null
    fi
    if ! pg_ctl -D "$DATA" status >/dev/null 2>&1; then
        # TCP on localhost only; a Unix socket path inside a deep checkout can exceed the OS limit.
        pg_ctl -D "$DATA" -l "$DATA/server.log" -w \
            -o "-p $PORT -c listen_addresses=localhost -c unix_socket_directories=''" start >/dev/null
    fi
    $PSQL -d postgres -tc "SELECT 1 FROM pg_database WHERE datname = 'triage'" | grep -q 1 ||
        $PSQL -d postgres -c "CREATE DATABASE triage"
    PGOPTIONS='-c client_min_messages=warning' $PSQL -d triage -f db/01-schema.sql -f db/02-seed.sql
    echo "postgres ready: DATABASE_URL=postgres://triage@localhost:$PORT/triage"
    ;;
stop)
    pg_ctl -D "$DATA" -w stop >/dev/null && echo "postgres stopped"
    ;;
*)
    echo "usage: $0 [start|stop]" >&2
    exit 2
    ;;
esac
