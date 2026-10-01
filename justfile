set dotenv-load

# Local dev Postgres (`just pg`) listens here; override by setting DATABASE_URL (e.g. in .env).
export DATABASE_URL := env_var_or_default("DATABASE_URL", "postgres://postgres@127.0.0.1:5433/money?sslmode=disable")
# Local server port, loopback only (other local projects hold 8080/5173; the dev server has login off). Vite on 5188 proxies here.
export ADDR := env_var_or_default("ADDR", "127.0.0.1:8188")

default:
    just --list

setup:
    cd web && npm install
    mkdir -p web/dist && touch web/dist/.keep

dev: setup pg
    #!/usr/bin/env bash
    set -e
    trap 'kill 0' EXIT
    AUTH_DISABLED=true watchexec -r -e go,sql -- go run . &
    npm --prefix web run dev &
    echo 'Open http://localhost:5188'
    wait

# Walk through first-time setup: secrets into 1Password, SimpleFIN claim, .env.op for the recipes below.
bootstrap:
    python3 scripts/bootstrap.py

# `just dev` against your real SimpleFIN data; secrets come from 1Password via op run (login off).
dev-op:
    @test -f .env.op || { echo 'Run `just bootstrap` first.'; exit 1; }
    op run --env-file .env.op -- just dev

# `just dev` with SimpleFIN's public demo data in a separate money_demo database; needs no secrets.
demo: pg
    #!/usr/bin/env bash
    set -euo pipefail
    psql -h 127.0.0.1 -p 5433 -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname='money_demo'" | grep -q 1 || createdb -h 127.0.0.1 -p 5433 -U postgres money_demo
    DATABASE_URL="postgres://postgres@127.0.0.1:5433/money_demo?sslmode=disable" SIMPLEFIN_ACCESS_URL="https://demo:demo@beta-bridge.simplefin.org/simplefin" just dev

# Production-like: the built binary on 127.0.0.1:8188 with real Pocket ID login and your data.
local: build pg
    @test -f .env.op -a -f .env.op.auth || { echo 'Run `just bootstrap` first.'; exit 1; }
    BACKUP_DIR=data/backups op run --env-file .env.op --env-file .env.op.auth -- ./bin/money-tracker

build: setup
    npm --prefix web run build
    mkdir -p bin
    CGO_ENABLED=0 go build -o bin/money-tracker .

# Start the local dev Postgres in data/pg (127.0.0.1:5433, trust auth, no unix socket) and create the money database.
pg:
    #!/usr/bin/env bash
    set -euo pipefail
    [ -f data/pg/PG_VERSION ] || initdb -D data/pg -U postgres --auth=trust -E UTF8 >/dev/null
    pg_ctl -D data/pg status >/dev/null 2>&1 || pg_ctl -D data/pg -l data/pg.log -w -o "-p 5433 -c listen_addresses=127.0.0.1 -c unix_socket_directories=" start >/dev/null
    psql -h 127.0.0.1 -p 5433 -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname='money'" | grep -q 1 || createdb -h 127.0.0.1 -p 5433 -U postgres money
    echo "postgres up: $DATABASE_URL"

pg-stop:
    pg_ctl -D data/pg stop

# Runs the tests against TEST_DATABASE_URL if set, else against a throwaway Postgres in data/pg-test (port 5434), stopped afterwards.
test *args:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -z "${TEST_DATABASE_URL:-}" ]; then
      [ -f data/pg-test/PG_VERSION ] || initdb -D data/pg-test -U postgres --auth=trust -E UTF8 >/dev/null
      if ! pg_ctl -D data/pg-test status >/dev/null 2>&1; then
        pg_ctl -D data/pg-test -l data/pg-test.log -w -o "-p 5434 -c listen_addresses=127.0.0.1 -c unix_socket_directories= -c fsync=off" start >/dev/null
        trap 'pg_ctl -D data/pg-test stop >/dev/null' EXIT
      fi
      psql -h 127.0.0.1 -p 5434 -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname='money_test'" | grep -q 1 || createdb -h 127.0.0.1 -p 5434 -U postgres money_test
      export TEST_DATABASE_URL="postgres://postgres@127.0.0.1:5434/money_test?sslmode=disable"
    fi
    go vet ./...
    go test {{args}} ./...

# Redeem a SimpleFIN setup token once, e.g. `pbpaste | just simplefin-claim`; prints only the access URL.
simplefin-claim:
    go run . simplefin-claim

docker:
    docker build -t money-tracker .

db:
    psql "$DATABASE_URL"
