#!/bin/sh
# Runs the end-to-end test and the whole crash matrix (tools/e2e) against
# MariaDB and PostgreSQL in Docker, then removes the containers:
#
#   tools/e2e/docker.sh                      # from the repository root
#   MARIADB=mariadb:10.6 POSTGRES=postgres:13 tools/e2e/docker.sh
#
# PREFIX names the containers (default dg-e2e), PORT the first of the two
# local ports (default 35301).
set -eu
prefix=${PREFIX:-dg-e2e}
port=${PORT:-35301}
mariadb=${MARIADB:-mariadb:11.4}
postgres=${POSTGRES:-postgres:17}

cleanup() { docker rm -f "$prefix-mariadb" "$prefix-postgres" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM
cleanup
docker run -d --name "$prefix-mariadb" --memory 1g -p "127.0.0.1:$port:3306" \
	-e MARIADB_ROOT_PASSWORD=dg -e MYSQL_ROOT_PASSWORD=dg "$mariadb" >/dev/null
docker run -d --name "$prefix-postgres" --memory 1g -p "127.0.0.1:$((port + 1)):5432" \
	-e POSTGRES_PASSWORD=dg "$postgres" >/dev/null
for i in $(seq 120); do
	if docker exec "$prefix-mariadb" sh -c 'mariadb -uroot -pdg -e "SELECT 1" || mysql -uroot -pdg -e "SELECT 1"' >/dev/null 2>&1 &&
		docker exec "$prefix-postgres" pg_isready -U postgres >/dev/null 2>&1; then
		break
	fi
	sleep 1
done

export DRIFTGATE_TEST_MYSQL="mysql://root:dg@127.0.0.1:$port/"
export DRIFTGATE_TEST_POSTGRES="postgres://postgres:dg@127.0.0.1:$((port + 1))/postgres"
export DRIFTGATE_E2E=1 DRIFTGATE_CHAOS=full
start=$(date +%s)
go test -count=1 -v ./tools/e2e/
echo "tools/e2e: $(($(date +%s) - start)) s"
