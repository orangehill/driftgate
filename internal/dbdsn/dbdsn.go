// Package dbdsn reads the DSNs of MySQL/MariaDB and PostgreSQL databases the
// way their drivers do, for the state components and the data module alike.
// A DSN holds a password, so no error of this package carries the DSN or
// any part of it: a parse error of net/url or of a driver quotes the text it
// could not read, which may be the password itself.
package dbdsn

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// Drivers.
const (
	MySQL    = "mysql" // MySQL and MariaDB
	Postgres = "postgres"
)

// errURL replaces a parse error of a URL, which would quote the DSN.
var errURL = errors.New("dsn: not a valid URL (the DSN is not shown, as it holds the password); percent-encode special characters such as @ : / % in the user name and password")

// MySQLConfig accepts mysql://user:pass@host:port/ (or mariadb://) and the
// driver's own form (user:pass@tcp(host:port)/). The database in it is
// ignored. The URL takes the parameters of mysqlParams; any other is an
// error, so a connection is never made without the TLS it asked for. The
// driver's form takes the driver's own parameters. DriftGate's connections
// use the text protocol: no server-side prepared statements, so every value
// arrives as its text and Com_stmt_execute is left to the application, and
// no multi-statements.
func MySQLConfig(dsn string) (*mysql.Config, error) {
	var cfg *mysql.Config
	if strings.HasPrefix(dsn, "mysql://") || strings.HasPrefix(dsn, "mariadb://") {
		u, err := url.Parse(dsn)
		if err != nil || u.Fragment != "" {
			return nil, errURL
		}
		cfg = mysql.NewConfig()
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
		cfg.Net, cfg.Addr = "tcp", u.Host
		if u.Port() == "" {
			cfg.Addr = u.Host + ":3306"
		}
		if err := mysqlParams(cfg, u.Query()); err != nil {
			return nil, err
		}
	} else {
		var err error
		if cfg, err = mysql.ParseDSN(dsn); err != nil {
			return nil, errors.New("dsn: the MySQL driver cannot read it (the DSN is not shown, as it holds the password); use mysql://user:pass@host:port/ or user:pass@tcp(host:port)/")
		}
	}
	cfg.DBName = ""
	cfg.MultiStatements = false
	cfg.InterpolateParams = true
	cfg.ParseTime = false
	return cfg, nil
}

// mysqlParams sets the parameters of a mysql:// URL on cfg:
//
//   - tls: true (verify the server's certificate and name), skip-verify
//     (encrypt without verifying), preferred (encrypt when the server can,
//     without verifying) or false, as the driver reads them;
//   - ssl-mode, as the mysql client names it: DISABLED, PREFERRED, REQUIRED
//     (encrypt without verifying), VERIFY_CA and VERIFY_IDENTITY (both
//     verify the certificate and the name);
//   - ssl-ca: a PEM file of the certificate authorities the server's
//     certificate is verified with (implies verification);
//   - timeout, readTimeout, writeTimeout: Go durations (10s).
//
// No error quotes a value, and a parameter is named only when its name
// cannot hold a part of the password (a ? in it starts the query).
func mysqlParams(cfg *mysql.Config, q url.Values) error {
	var mode, ca string
	for key, vals := range q {
		v := vals[len(vals)-1]
		switch key {
		case "tls":
			switch v {
			case "true", "false", "skip-verify", "preferred":
				mode = v
			default:
				return errors.New("dsn: tls must be true, false, skip-verify or preferred")
			}
		case "ssl-mode":
			switch strings.ToUpper(v) {
			case "DISABLED":
				mode = "false"
			case "PREFERRED":
				mode = "preferred"
			case "REQUIRED":
				mode = "skip-verify"
			case "VERIFY_CA", "VERIFY_IDENTITY":
				mode = "true"
			default:
				return errors.New("dsn: ssl-mode must be DISABLED, PREFERRED, REQUIRED, VERIFY_CA or VERIFY_IDENTITY")
			}
		case "ssl-ca":
			ca = v
		case "timeout", "readTimeout", "writeTimeout":
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return fmt.Errorf("dsn: %s must be a duration such as 10s", key)
			}
			switch key {
			case "timeout":
				cfg.Timeout = d
			case "readTimeout":
				cfg.ReadTimeout = d
			default:
				cfg.WriteTimeout = d
			}
		default:
			name := "a parameter"
			if strings.Trim(key, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") == "" && len(key) <= 40 {
				name = fmt.Sprintf("parameter %q", key)
			}
			return fmt.Errorf("dsn: %s of the mysql:// URL is not supported (tls, ssl-mode, ssl-ca, timeout, readTimeout, writeTimeout; the DSN is not shown, as it holds the password)", name)
		}
	}
	if ca != "" {
		if mode == "false" || mode == "skip-verify" || mode == "preferred" {
			return errors.New("dsn: ssl-ca verifies the server's certificate, which this tls or ssl-mode turns off")
		}
		pem, err := os.ReadFile(ca)
		if err != nil {
			return fmt.Errorf("dsn: ssl-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("dsn: ssl-ca: no PEM certificate in %s", ca)
		}
		cfg.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		return nil
	}
	cfg.TLSConfig = mode
	return nil
}

// PostgresURL reads a postgres:// (or postgresql://) URL; a URL without a
// database gets postgres.
func PostgresURL(dsn string) (*url.URL, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, errURL
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, errors.New("dsn: a postgres:// URL is required")
	}
	if strings.Trim(u.Path, "/") == "" {
		u.Path = "/postgres"
	}
	return u, nil
}

// Hosts lists every host the driver may connect to with the DSN, as the
// driver itself reads it (PostgreSQL: several hosts, fallbacks, host=
// parameters, PG* variables); "" stands for a local socket.
func Hosts(driver, dsn string) ([]string, error) {
	if driver == Postgres {
		u, err := PostgresURL(dsn)
		if err != nil {
			return nil, err
		}
		cfg, err := pgconn.ParseConfig(u.String())
		if err != nil {
			// pgconn's error quotes the DSN, with the password masked only
			// when it is written plainly.
			return nil, errors.New("dsn: the PostgreSQL driver cannot read it (the DSN is not shown, as it holds the password)")
		}
		var out []string
		seen := map[string]bool{}
		for _, h := range append([]*pgconn.FallbackConfig{{Host: cfg.Host, Port: cfg.Port}}, cfg.Fallbacks...) {
			host := h.Host
			if network, _ := pgconn.NetworkAddress(h.Host, h.Port); network == "unix" {
				host = ""
			}
			if !seen[host] { // sslmode=prefer tries each host twice
				seen[host] = true
				out = append(out, host)
			}
		}
		return out, nil
	}
	cfg, err := MySQLConfig(dsn)
	if err != nil {
		return nil, err
	}
	switch cfg.Net {
	case "unix":
		return []string{""}, nil
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("dsn: unknown network %q", cfg.Net)
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		host = cfg.Addr
	}
	if host == "" {
		host = "localhost" // the driver dials the local system
	}
	return []string{host}, nil
}

// Server names the server a DSN connects to, without the user or password:
// the network and address for MySQL (tcp(127.0.0.1:3306), unix(/tmp/s)),
// every host:port in order for PostgreSQL. Two DSNs with the same server
// reach the same databases.
func Server(driver, dsn string) (string, error) {
	if driver == Postgres {
		u, err := PostgresURL(dsn)
		if err != nil {
			return "", err
		}
		cfg, err := pgconn.ParseConfig(u.String())
		if err != nil {
			return "", errors.New("dsn: the PostgreSQL driver cannot read it (the DSN is not shown, as it holds the password)")
		}
		addrs := []string{net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))}
		for _, f := range cfg.Fallbacks {
			if a := net.JoinHostPort(f.Host, fmt.Sprint(f.Port)); a != addrs[len(addrs)-1] { // sslmode=prefer tries each host twice
				addrs = append(addrs, a)
			}
		}
		return strings.Join(addrs, ","), nil
	}
	cfg, err := MySQLConfig(dsn)
	if err != nil {
		return "", err
	}
	return cfg.Net + "(" + cfg.Addr + ")", nil
}
