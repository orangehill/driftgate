#!/bin/sh
# Checks DriftGate's TLS to SQL Server against a server with a certificate
# of its own, which the stock image does not have: a CA and a server
# certificate for localhost are made here, and SQL Server (in Docker) is
# set up to use it for every connection (forceencryption). driftgate seed
# then connects with the DSNs a user writes:
#
#   - verified against the CA (encrypt=true, certificate=<CA file>): works;
#   - against the system's CAs, at a host the certificate does not name,
#     or naming another host (hostNameInCertificate): refused, the password
#     in no message;
#   - TDS 8 (encrypt=strict) on SQL Server 2025 and later: works, and is
#     refused at a host the certificate does not name; before (2022 on
#     Linux has no TDS 8), refused, never weakened.
#
#   tools/e2e/sqlserver-tls.sh               # from the repository root
#   IMAGE=mcr.microsoft.com/mssql/server:2025-latest tools/e2e/sqlserver-tls.sh
#
# NAME names the container (default dg-mssql-tls), PORT its local port
# (default 14332); both are removed at the end with everything made here.
set -eu
image=${IMAGE:-mcr.microsoft.com/mssql/server:2022-latest}
name=${NAME:-dg-mssql-tls}
port=${PORT:-14332}
pw=Dg-Tls-Passw0rd-2026
dir=$(mktemp -d)

cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	rm -rf "$dir"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
docker rm -f "$name" >/dev/null 2>&1 || true

# The CA, and the server's certificate for localhost only (no IP address).
cat >"$dir/openssl.cnf" <<'EOF'
[req]
distinguished_name = dn
prompt = no
[dn]
CN = DriftGate test CA
[ca]
basicConstraints = critical, CA:true
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
[server]
basicConstraints = CA:false
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:localhost
EOF
openssl req -x509 -new -newkey rsa:2048 -nodes -days 2 -config "$dir/openssl.cnf" -extensions ca \
	-keyout "$dir/ca.key" -out "$dir/ca.pem" 2>/dev/null
openssl req -new -newkey rsa:2048 -nodes -subj /CN=localhost -keyout "$dir/server.key" -out "$dir/server.csr" 2>/dev/null
openssl x509 -req -in "$dir/server.csr" -CA "$dir/ca.pem" -CAkey "$dir/ca.key" -CAcreateserial -days 2 \
	-extfile "$dir/openssl.cnf" -extensions server -out "$dir/server.pem" 2>/dev/null
chmod 644 "$dir/server.key" # read by the server's own user in the container
cat >"$dir/mssql.conf" <<'EOF'
[network]
tlscert = /var/opt/mssql/tls/server.pem
tlskey = /var/opt/mssql/tls/server.key
tlsprotocols = 1.2
forceencryption = 1
EOF
mkdir "$dir/tls"
cp "$dir/server.pem" "$dir/server.key" "$dir/tls/"

docker create --name "$name" --platform linux/amd64 --memory 2g -p "127.0.0.1:$port:1433" \
	-e ACCEPT_EULA=Y -e MSSQL_PID=Developer -e MSSQL_SA_PASSWORD="$pw" "$image" >/dev/null
# SQL Server 2017's image makes /var/opt/mssql only when it starts (and
# runs as root): the folder is copied whole there.
if docker cp "$dir/tls" "$name:/var/opt/mssql/tls" 2>/dev/null; then
	docker cp "$dir/mssql.conf" "$name:/var/opt/mssql/mssql.conf"
else
	mkdir "$dir/var"
	cp -R "$dir/tls" "$dir/mssql.conf" "$dir/var/"
	docker cp "$dir/var" "$name:/var/opt/mssql"
fi
docker start "$name" >/dev/null

sqlcmd() {
	docker exec "$name" sh -c 'if [ -x /opt/mssql-tools18/bin/sqlcmd ]; then c=/opt/mssql-tools18/bin/sqlcmd; else c=/opt/mssql-tools/bin/sqlcmd; fi
		exec $c -C -S localhost -U sa -P "$0" -b -h -1 -W -Q "SET NOCOUNT ON; $1"' "$pw" "$1"
}
ready=
for i in $(seq 120); do
	if sqlcmd "SELECT 1" >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 1
done
if [ -z "$ready" ]; then
	docker logs "$name" | tail -30
	echo "SQL Server did not start" >&2
	exit 1
fi
major=$(sqlcmd "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)" | tr -d '\r ')
echo "SQL Server $major ($image), certificate of the test CA for localhost, forceencryption"
sqlcmd "CREATE DATABASE dg_tls" >/dev/null
sqlcmd "CREATE TABLE dg_tls.dbo.t (id int PRIMARY KEY); INSERT INTO dg_tls.dbo.t VALUES (1)" >/dev/null

go build -o "$dir/driftgate" ./cmd/driftgate
mkdir -p "$dir/project/driftgate"
cat >"$dir/project/driftgate.yaml" <<'EOF'
version: 1
project: tls
target:
  base_url: http://127.0.0.1:1
state:
  db:
    driver: sqlserver
    dsn: { env: DG_TLS_DSN }
    working: dg_tls
    pristine: dg_tls_pristine
EOF
key=$(head -c 32 /dev/urandom | base64)

failed=0
# check <want: ok|refused> <what> <query> [host] [message the refusal names]
check() {
	want=$1 what=$2 query=$3 host=${4:-localhost} says=${5:-}
	if out=$(cd "$dir/project" && DG_TLS_DSN="sqlserver://sa:$pw@$host:$port?$query" DRIFTGATE_STORE="$dir/store" \
		DRIFTGATE_KEY="$key" "$dir/driftgate" seed 2>&1); then
		got=ok
	else
		got=refused
	fi
	case "$out" in *"$pw"*)
		echo "FAIL $what: the password in the output"
		failed=1
		;;
	esac
	if [ "$got" != "$want" ] || { [ "$got" = refused ] && [ -n "$says" ] && ! printf '%s' "$out" | grep -q "$says"; }; then
		echo "FAIL $what: $got, want $want${says:+ ($says)}"
		printf '%s\n' "$out" | sed 's/^/    /'
		failed=1
		return
	fi
	echo "ok   $what: $got"
}
ca="certificate=$dir/ca.pem"
check ok "verified against the CA" "encrypt=true&$ca"
check ok "verified, the certificate's host named for an address" "encrypt=true&$ca&hostNameInCertificate=localhost" 127.0.0.1
check refused "the system's CAs" "encrypt=true" localhost "certificate"
check refused "an address the certificate does not name" "encrypt=true&$ca" 127.0.0.1 "certificate"
check refused "another host named" "encrypt=true&$ca&hostNameInCertificate=wrong.example" localhost "certificate"
check refused "encrypt=false still verifies the sign-in's certificate" "encrypt=false" localhost "certificate"
# SQL Server on Linux takes TDS 8 from 2025 (17) on; 2022 and 2019 close
# such a connection ("A valid TLS certificate is not configured to accept
# strict (TDS 8.0 and above) connections"), and DriftGate never falls back
# to a weaker encryption.
if [ "$major" -ge 17 ]; then
	check ok "TDS 8 (strict) verified against the CA" "encrypt=strict&$ca"
	check refused "TDS 8 at an address the certificate does not name" "encrypt=strict&$ca" 127.0.0.1 "certificate"
	check refused "TDS 8 ignores TrustServerCertificate" "encrypt=strict&TrustServerCertificate=true" 127.0.0.1 "certificate"
else
	check refused "TDS 8 (strict), which SQL Server $major on Linux does not take" "encrypt=strict&$ca" localhost "TLS Handshake failed"
fi
# The server encrypted the connections: forceencryption, with the test
# certificate (the verified checks above passed against the test CA).
enc=$(sqlcmd "SELECT COUNT(*) FROM sys.dm_exec_connections WHERE encrypt_option = 'FALSE' AND net_transport = 'TCP'" | tr -d '\r ')
if [ "$enc" != 0 ]; then
	echo "FAIL $enc unencrypted TCP connections"
	failed=1
fi
exit $failed
