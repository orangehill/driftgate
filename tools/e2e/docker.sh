#!/bin/sh
# Runs the end-to-end test and the whole crash matrix (tools/e2e) against
# MariaDB and PostgreSQL in Docker, then removes the containers:
#
#   tools/e2e/docker.sh                      # from the repository root
#   MARIADB=mariadb:10.6 POSTGRES=postgres:13 tools/e2e/docker.sh
#
# SQLSERVER adds SQL Server, whose tests run too (an x86-64 image: on Apple
# silicon, Docker runs it under Rosetta, which must be on):
#
#   SQLSERVER=mcr.microsoft.com/mssql/server:2022-latest tools/e2e/docker.sh
#
# PREFIX names the containers (default dg-e2e), PORT the first of the
# local ports (default 35301; SQL Server takes PORT+2).
set -eu
prefix=${PREFIX:-dg-e2e}
port=${PORT:-35301}
mariadb=${MARIADB:-mariadb:11.4}
postgres=${POSTGRES:-postgres:17}
sqlserver=${SQLSERVER:-}
sapw=Dg-Passw0rd-e2e

cleanup() { docker rm -f "$prefix-mariadb" "$prefix-postgres" "$prefix-mssql" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM
cleanup
docker run -d --name "$prefix-mariadb" --memory 1g -p "127.0.0.1:$port:3306" \
	-e MARIADB_ROOT_PASSWORD=dg -e MYSQL_ROOT_PASSWORD=dg "$mariadb" >/dev/null
docker run -d --name "$prefix-postgres" --memory 1g -p "127.0.0.1:$((port + 1)):5432" \
	-e POSTGRES_PASSWORD=dg "$postgres" >/dev/null
if [ -n "$sqlserver" ]; then
	docker run -d --name "$prefix-mssql" --platform linux/amd64 --memory 2g -p "127.0.0.1:$((port + 2)):1433" \
		-e ACCEPT_EULA=Y -e MSSQL_PID=Developer -e MSSQL_SA_PASSWORD="$sapw" "$sqlserver" >/dev/null
fi
# sqlcmd is in mssql-tools18 in current images, mssql-tools in older ones.
mssql_ready() {
	[ -z "$sqlserver" ] || docker exec "$prefix-mssql" sh -c 'if [ -x /opt/mssql-tools18/bin/sqlcmd ]; then
		exec /opt/mssql-tools18/bin/sqlcmd -C -S localhost -U sa -P "$0" -Q "SELECT 1" -b
	fi
	exec /opt/mssql-tools/bin/sqlcmd -S localhost -U sa -P "$0" -Q "SELECT 1" -b' "$sapw" >/dev/null 2>&1
}
for i in $(seq 180); do
	if docker exec "$prefix-mariadb" sh -c 'mariadb -uroot -pdg -e "SELECT 1" || mysql -uroot -pdg -e "SELECT 1"' >/dev/null 2>&1 &&
		docker exec "$prefix-postgres" pg_isready -U postgres >/dev/null 2>&1 && mssql_ready; then
		break
	fi
	sleep 1
done

export DRIFTGATE_TEST_MYSQL="mysql://root:dg@127.0.0.1:$port/"
export DRIFTGATE_TEST_POSTGRES="postgres://postgres:dg@127.0.0.1:$((port + 1))/postgres"
if [ -n "$sqlserver" ]; then
	export DRIFTGATE_TEST_SQLSERVER="sqlserver://sa:$sapw@127.0.0.1:$((port + 2))?encrypt=true&TrustServerCertificate=true"
fi
export DRIFTGATE_E2E=1 DRIFTGATE_CHAOS=full
start=$(date +%s)
go test -count=1 -v ./tools/e2e/
echo "tools/e2e: $(($(date +%s) - start)) s"
