package appconf

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// options are the settings of an application's DSN besides the address
// and the credentials that DriftGate passes on: TLS, and for PostgreSQL the
// connect timeout, the application name and the kind of session. A TLS
// setting is never dropped: one DriftGate cannot pass on is an error, so a
// connection never goes without the TLS the application asked for.
type options struct {
	mode                           string // disable, prefer, require, verify-ca, verify-full; "" unset
	ssl                            string // pgJDBC's ssl=true
	useSSL, requireSSL, verifyCert string // Connector/J's flags
	ca, cert, key                  string
	prisma                         bool // Prisma's sslcert is the server's CA
	timeout, appName, sessionAttrs string
}

// sslMode reads a TLS mode as libpq, the mysql client, MySqlConnector,
// Npgsql and Connector/J write it.
func sslMode(v string) (string, bool) {
	switch fieldKey(v) {
	case "disable", "disabled", "none", "false", "off", "0":
		return "disable", true
	case "allow", "prefer", "preferred":
		return "prefer", true
	case "require", "required", "true", "on", "1":
		return "require", true
	case "verifyca":
		return "verify-ca", true
	case "verifyfull", "verifyidentity":
		return "verify-full", true
	}
	return "", false
}

// errTLS names a TLS setting whose value DriftGate does not know; the key
// is one of the names set knows, so it holds no part of a password.
func errTLS(key string) error {
	return fmt.Errorf("the TLS setting %s has a value DriftGate does not know (the value is not shown); set it to one of disable, prefer, require, verify-ca, verify-full", key)
}

// set records one setting, by its key in any spelling the drivers use
// (sslmode, ssl-mode, SslMode, SSL Mode); a key it does not know is left
// out.
func (o *options) set(key, v string) error {
	v = strings.TrimSpace(v)
	switch k := fieldKey(key); k {
	case "sslmode":
		m, ok := sslMode(v)
		if !ok {
			return errTLS(k)
		}
		o.mode = m
	case "tls": // the Go MySQL driver
		switch strings.ToLower(v) {
		case "true":
			o.mode = "verify-full"
		case "skip-verify":
			o.mode = "require"
		case "preferred":
			o.mode = "prefer"
		case "false":
			o.mode = "disable"
		default:
			return errTLS(k)
		}
	case "ssl", "usessl", "requiressl", "verifyservercertificate":
		b := strings.ToLower(v)
		if b != "true" && b != "false" {
			return errTLS(k)
		}
		switch k {
		case "ssl":
			o.ssl = b
		case "usessl":
			o.useSSL = b
		case "requiressl":
			o.requireSSL = b
		default:
			o.verifyCert = b
		}
	case "sslaccept": // Prisma
		switch v {
		case "strict":
			o.prisma, o.mode = true, "verify-full"
		case "accept_invalid_certs":
			o.prisma, o.mode = true, "require"
		default:
			return errTLS(k)
		}
	case "sslidentity", "sslpassword": // Prisma's client identity (PKCS#12)
		return fmt.Errorf("the TLS setting %s (a client identity) is not supported", k)
	case "sslca", "sslrootcert", "rootcertificate", "cacertificatefile", "sslcafile":
		o.ca = v
	case "sslcert", "sslcertificate", "certificatefile", "clientcertificate":
		o.cert = v
	case "sslkey", "sslkeyfile", "certificatekeyfile":
		o.key = v
	case "connecttimeout", "connectiontimeout":
		if v != "" && strings.Trim(v, "0123456789") == "" {
			o.timeout = v
		}
	case "applicationname":
		o.appName = v
	case "targetsessionattrs":
		o.sessionAttrs = v
	case "targetservertype": // pgJDBC
		o.sessionAttrs = map[string]string{"any": "any", "primary": "primary", "master": "primary", "secondary": "standby", "slave": "standby", "prefersecondary": "prefer-standby", "preferslave": "prefer-standby"}[fieldKey(v)]
	}
	return nil
}

// tlsMode is the mode the settings ask for; "" when they leave it to the
// driver.
func (o *options) tlsMode() string {
	switch {
	case o.mode != "":
		return o.mode
	case o.ssl == "true":
		return "verify-full" // pgJDBC's ssl=true verifies the certificate and the host
	case o.ssl == "false", o.useSSL == "false":
		return "disable"
	case o.useSSL == "true", o.requireSSL == "true":
		if o.verifyCert == "true" {
			return "verify-ca"
		}
		return "require"
	}
	return ""
}

// params writes the settings as the DSN of driver takes them.
func (o *options) params(driver string) (url.Values, error) {
	if o.prisma && o.cert != "" && o.ca == "" {
		o.ca, o.cert = o.cert, ""
	}
	q := url.Values{}
	mode := o.tlsMode()
	if driver == Postgres {
		for k, v := range map[string]string{"sslmode": mode, "sslrootcert": o.ca, "sslcert": o.cert, "sslkey": o.key,
			"connect_timeout": o.timeout, "application_name": o.appName, "target_session_attrs": o.sessionAttrs} {
			if v != "" {
				q.Set(k, v)
			}
		}
		return q, nil
	}
	if o.cert != "" || o.key != "" {
		return nil, errors.New("a client certificate (sslcert, sslkey) is not supported for MySQL")
	}
	if o.ca != "" && (mode == "" || mode == "require") {
		mode = "verify-ca" // as the mysql client takes --ssl-ca
	}
	if o.ca != "" && (mode == "verify-ca" || mode == "verify-full") {
		q.Set("ssl-ca", o.ca)
	}
	if mode != "" {
		q.Set("ssl-mode", map[string]string{"disable": "DISABLED", "prefer": "PREFERRED", "require": "REQUIRED", "verify-ca": "VERIFY_CA", "verify-full": "VERIFY_IDENTITY"}[mode])
	}
	return q, nil
}
