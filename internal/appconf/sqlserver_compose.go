package appconf

import (
	"fmt"
	"strings"
)

// sqlServerImage reports a compose image that runs SQL Server
// (imageDriver): mcr.microsoft.com/mssql/server, mssql/rhel/server, the
// old microsoft/mssql-server-linux, and Azure SQL Edge (refused when
// read); repo is the image without tag or digest, in lower case.
func sqlServerImage(repo string) bool {
	for _, w := range []string{"mssql/server", "mssql/rhel/server", "mssql-server", "azure-sql-edge"} {
		if strings.Contains(repo, w) {
			return true
		}
	}
	return false
}

// sqlServerDatabase reads the SQL Server service name of a compose file
// (composeProject.database): the sa login with MSSQL_SA_PASSWORD (or the
// older SA_PASSWORD), at the port the service publishes for MSSQL_TCP_PORT
// (default 1433). The container's certificate is its own, self-signed one:
// DriftGate trusts it on a loopback address of this machine only.
func (cp *composeProject) sqlServerDatabase(root, name string, env Lookup) (parts, error) {
	image, _ := interpolate(cp.services[name].image, cp.vars)
	if strings.Contains(strings.ToLower(image), "azure-sql-edge") {
		return parts{}, fmt.Errorf("%s: service %s runs Azure SQL Edge, which Microsoft has retired; run mcr.microsoft.com/mssql/server", cp.file, printableName(name))
	}
	e, err := cp.environment(root, name, env)
	if err != nil {
		if strings.HasPrefix(err.Error(), "services.") {
			return parts{}, fmt.Errorf("%s: %w", cp.file, err)
		}
		return parts{}, err
	}
	for _, k := range []string{"MSSQL_SA_PASSWORD_FILE", "SA_PASSWORD_FILE"} {
		if _, ok := e[k]; ok {
			return parts{}, fmt.Errorf("%s: service %s takes its password from a file (%s); DriftGate does not read it", cp.file, printableName(name), k)
		}
	}
	p := parts{driver: SQLServer, user: "sa"}
	p.mssql.read, p.mssql.compose = true, true
	var ok bool
	if p.password, ok = e["MSSQL_SA_PASSWORD"]; !ok || p.password == "" {
		p.password = e["SA_PASSWORD"]
	}
	if p.password == "" {
		return parts{}, fmt.Errorf("%s: service %s sets no MSSQL_SA_PASSWORD", cp.file, printableName(name))
	}
	target := defaultPort(SQLServer)
	if v := strings.TrimSpace(e["MSSQL_TCP_PORT"]); v != "" {
		if strings.Trim(v, "0123456789") != "" {
			return parts{}, fmt.Errorf("%s: service %s: MSSQL_TCP_PORT is not a number", cp.file, printableName(name))
		}
		target = v
	}
	addr, ok, err := cp.published(name, target)
	if err != nil {
		return parts{}, fmt.Errorf("%s: services.%s.ports: %w", cp.file, printableName(name), err)
	}
	if !ok {
		return parts{}, fmt.Errorf("%s: service %s publishes no port for %s; add ports: [\"%s:%s\"] (or another free port on the left)", cp.file, printableName(name), target, target, target)
	}
	if err := p.setHost(addr); err != nil {
		return parts{}, err
	}
	return p, nil
}
