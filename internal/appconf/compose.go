package appconf

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// composeFiles are the names docker compose looks for, in its order.
var composeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// composeOverride is the file compose reads over file by default
// (compose.override.yaml over compose.yaml); "" for another file.
func composeOverrides(file string) []string {
	dir, name := path.Split(file)
	for _, n := range composeFiles {
		if name == n {
			base := strings.TrimSuffix(strings.TrimSuffix(n, ".yaml"), ".yml")
			return []string{dir + base + ".override.yaml", dir + base + ".override.yml"}
		}
	}
	return nil
}

// service is a service of a compose file, merged with its override.
type service struct {
	image, containerName string
	build                bool
	env                  map[string]*string // nil: from the shell
	envFiles             []envFile
	ports                []port
}

type envFile struct {
	path     string
	required bool
}

type port struct{ hostIP, published, target, short string }

type composeYAML struct {
	Services map[string]struct {
		Image         string      `yaml:"image"`
		ContainerName string      `yaml:"container_name"`
		Build         yaml.Node   `yaml:"build"`
		Environment   yaml.Node   `yaml:"environment"`
		EnvFile       yaml.Node   `yaml:"env_file"`
		Ports         []yaml.Node `yaml:"ports"`
	} `yaml:"services"`
}

// composeProject reads a compose file and its override, with variables
// interpolated as compose does it: from the environment, else the .env
// next to the file.
type composeProject struct {
	file     string
	dir      string
	services map[string]*service
	vars     Lookup
}

func readComposeProject(root, file string, env Lookup) (*composeProject, error) {
	file = path.Clean(strings.ReplaceAll(file, `\`, "/"))
	dir := path.Dir(file)
	dotenv := map[string]string{}
	if raw, err := readFile(root, path.Join(dir, ".env")); err == nil {
		dotenv, _ = parseDotenv(raw, env)
	}
	vars := func(name string) (string, bool) {
		if v, ok := env(name); ok {
			return v, true
		}
		v, ok := dotenv[name]
		return v, ok
	}
	cp := &composeProject{file: file, dir: dir, services: map[string]*service{}, vars: vars}
	files := []string{file}
	for _, o := range composeOverrides(file) {
		if _, err := readFile(root, o); err == nil {
			files = append(files, o)
			break
		}
	}
	for _, f := range files {
		raw, err := readFile(root, f)
		if err != nil {
			return nil, err
		}
		var y composeYAML
		if err := yaml.Unmarshal(raw, &y); err != nil {
			return nil, fmt.Errorf("%s: not a compose file (the content is not shown)", f)
		}
		for name, ys := range y.Services {
			s := cp.services[name]
			if s == nil {
				s = &service{env: map[string]*string{}}
				cp.services[name] = s
			}
			if ys.Image != "" {
				s.image = ys.Image
			}
			if ys.ContainerName != "" {
				s.containerName = ys.ContainerName
			}
			s.build = s.build || !ys.Build.IsZero()
			if err := composeEnv(&ys.Environment, s.env); err != nil {
				return nil, fmt.Errorf("%s: services.%s.environment: %w", f, printableName(name), err)
			}
			if err := composeEnvFiles(&ys.EnvFile, s); err != nil {
				return nil, fmt.Errorf("%s: services.%s.env_file: %w", f, printableName(name), err)
			}
			for _, pn := range ys.Ports {
				p, ok := composePort(&pn)
				if !ok {
					continue
				}
				s.ports = append(s.ports, p)
			}
		}
	}
	return cp, nil
}

func composeEnv(n *yaml.Node, into map[string]*string) error {
	switch n.Kind {
	case 0:
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			if v.Tag == "!!null" {
				into[k] = nil
				continue
			}
			val := v.Value
			into[k] = &val
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			k, v, ok := strings.Cut(item.Value, "=")
			if !ok {
				into[k] = nil
				continue
			}
			into[k] = &v
		}
	default:
		return errors.New("neither a map nor a list")
	}
	return nil
}

func composeEnvFiles(n *yaml.Node, s *service) error {
	switch n.Kind {
	case 0:
	case yaml.ScalarNode:
		s.envFiles = append(s.envFiles, envFile{path: n.Value, required: true})
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if item.Kind == yaml.ScalarNode {
				s.envFiles = append(s.envFiles, envFile{path: item.Value, required: true})
				continue
			}
			var long struct {
				Path     string `yaml:"path"`
				Required *bool  `yaml:"required"`
			}
			if err := item.Decode(&long); err != nil {
				return errors.New("an item is neither a path nor {path, required}")
			}
			s.envFiles = append(s.envFiles, envFile{path: long.Path, required: long.Required == nil || *long.Required})
		}
	default:
		return errors.New("neither a path nor a list")
	}
	return nil
}

// composePort reads a port of the long form ({target, published,
// host_ip}); one of the short form is kept as it is written, to be read
// after interpolation (shortPort).
func composePort(n *yaml.Node) (port, bool) {
	if n.Kind == yaml.MappingNode {
		var long struct {
			Target    string `yaml:"target"`
			Published string `yaml:"published"`
			HostIP    string `yaml:"host_ip"`
		}
		if n.Decode(&long) != nil || long.Published == "" {
			return port{}, false
		}
		return port{hostIP: long.HostIP, published: long.Published, target: long.Target}, true
	}
	return port{short: n.Value}, n.Kind == yaml.ScalarNode
}

// shortPort reads a port of the short form: [host_ip:]published:target
// [/protocol]. A port without a fixed published one is not.
func shortPort(s string) (port, bool) {
	s, _, _ = strings.Cut(s, "/")
	var ip string
	if strings.HasPrefix(s, "[") { // [::1]:3307:3306
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return port{}, false
		}
		ip, s = s[1:end], strings.TrimPrefix(s[end+1:], ":")
	}
	f := strings.Split(s, ":")
	switch len(f) {
	case 2:
		return port{hostIP: ip, published: f[0], target: f[1]}, true
	case 3:
		return port{hostIP: f[0], published: f[1], target: f[2]}, true
	}
	return port{}, false
}

// interpolate expands compose's variables: $NAME, ${NAME}, ${NAME:-d},
// ${NAME-d}, ${NAME:+r}, ${NAME+r}, ${NAME:?e}, ${NAME?e}, and $$ for $.
func interpolate(s string, vars Lookup) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch c := s[i+1]; {
		case c == '$':
			b.WriteByte('$')
			i++
		case c == '{':
			depth, end := 0, -1
			for j := i + 1; j < len(s) && end < 0; j++ {
				switch s[j] {
				case '{':
					depth++
				case '}':
					if depth--; depth == 0 {
						end = j
					}
				}
			}
			if end < 0 {
				return "", errors.New("${ without }")
			}
			inner := s[i+2 : end]
			n := 0
			for n < len(inner) && (inner[n] == '_' || 'a' <= inner[n] && inner[n] <= 'z' || 'A' <= inner[n] && inner[n] <= 'Z' || n > 0 && '0' <= inner[n] && inner[n] <= '9') {
				n++
			}
			name, op := inner[:n], inner[n:]
			v, set := vars(name)
			colon := strings.HasPrefix(op, ":")
			op = strings.TrimPrefix(op, ":")
			empty := !set || colon && v == ""
			var arg string
			if len(op) > 0 {
				var err error
				if arg, err = interpolate(op[1:], vars); err != nil {
					return "", err
				}
			}
			switch {
			case op == "":
			case op[0] == '-':
				if empty {
					v = arg
				}
			case op[0] == '+':
				v = ""
				if !empty {
					v = arg
				}
			case op[0] == '?':
				if empty {
					return "", fmt.Errorf("variable %s is required and not set", printableName(name))
				}
			default:
				return "", errors.New("invalid ${…}")
			}
			b.WriteString(v)
			i = end
		case c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
			j := i + 1
			for j < len(s) && isEnvByte(s[j]) {
				j++
			}
			v, _ := vars(s[i+1 : j])
			b.WriteString(v)
			i = j - 1
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), nil
}

func isEnvByte(c byte) bool {
	return c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// environment is the environment of a service: its env_file files in
// order, then environment.
func (cp *composeProject) environment(root, name string, env Lookup) (map[string]string, error) {
	s := cp.services[name]
	out := map[string]string{}
	for _, ef := range s.envFiles {
		p, err := interpolate(ef.path, cp.vars)
		if err != nil {
			return nil, fmt.Errorf("services.%s.env_file: %w", printableName(name), err)
		}
		f := path.Join(cp.dir, strings.ReplaceAll(p, `\`, "/"))
		if err := CheckFile(f); err != nil {
			return nil, fmt.Errorf("services.%s.env_file: %w", printableName(name), err)
		}
		raw, err := readFile(root, f)
		if err != nil {
			if !ef.required {
				continue
			}
			return nil, err
		}
		m, err := parseDotenv(raw, cp.vars)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for k, v := range m {
			out[k] = v
		}
	}
	for k, v := range s.env {
		if v == nil {
			if sv, ok := env(k); ok {
				out[k] = sv
			}
			continue
		}
		iv, err := interpolate(*v, cp.vars)
		if err != nil {
			return nil, fmt.Errorf("services.%s.environment.%s: %w", printableName(name), printableName(k), err)
		}
		out[k] = iv
	}
	return out, nil
}

// imageDriver is the database an image runs: mysql, mariadb, percona,
// postgres, postgis and timescaledb images (official or not), and SQL
// Server's (sqlServerImage).
func imageDriver(image string) string {
	repo := image
	if i := strings.IndexByte(repo, '@'); i >= 0 {
		repo = repo[:i]
	}
	if i := strings.LastIndexByte(repo, ':'); i >= 0 && !strings.Contains(repo[i:], "/") {
		repo = repo[:i]
	}
	repo = strings.ToLower(repo)
	for _, w := range []string{"mysql", "mariadb", "percona"} {
		if strings.Contains(repo, w) {
			return MySQL
		}
	}
	for _, w := range []string{"postgres", "postgis", "timescale"} {
		if strings.Contains(repo, w) {
			return Postgres
		}
	}
	if sqlServerImage(repo) {
		return SQLServer
	}
	return ""
}

// published is the address of this machine at which the service's port
// target is published, and whether it is.
func (cp *composeProject) published(name, target string) (string, bool, error) {
	for _, p := range cp.services[name].ports {
		if p.short != "" {
			s, err := interpolate(p.short, cp.vars)
			if err != nil {
				return "", false, err
			}
			var ok bool
			if p, ok = shortPort(s); !ok {
				continue
			}
		}
		t, err := interpolate(p.target, cp.vars)
		if err != nil {
			return "", false, err
		}
		if t != target {
			continue
		}
		pub, err := interpolate(p.published, cp.vars)
		if err != nil {
			return "", false, err
		}
		ip, err := interpolate(p.hostIP, cp.vars)
		if err != nil {
			return "", false, err
		}
		if pub == "" || strings.Trim(pub, "0123456789") != "" { // a range: no fixed port
			continue
		}
		if ip == "" || ip == "0.0.0.0" || ip == "::" {
			ip = "127.0.0.1"
		}
		return joinHost(ip, pub), true, nil
	}
	return "", false, nil
}

func joinHost(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// readCompose reads the database service key (its name) of a compose file:
// the port it publishes on this machine and the user its image creates.
// MySQL and MariaDB: root when MYSQL_ROOT_PASSWORD (MARIADB_ROOT_PASSWORD)
// or an empty root password is set, as DriftGate creates the pristine
// database; else MYSQL_USER with MYSQL_PASSWORD. PostgreSQL: POSTGRES_USER
// (default postgres) with POSTGRES_PASSWORD.
func readCompose(root, file, key string, env Lookup) (parts, error) {
	if key == "" {
		return parts{}, fmt.Errorf("%s: needs key (the name of the database service)", file)
	}
	cp, err := readComposeProject(root, file, env)
	if err != nil {
		return parts{}, err
	}
	return cp.database(root, key, env)
}

func (cp *composeProject) database(root, name string, env Lookup) (parts, error) {
	s, ok := cp.services[name]
	if !ok {
		return parts{}, fmt.Errorf("%s: no service %s", cp.file, printableName(name))
	}
	image, err := interpolate(s.image, cp.vars)
	if err != nil {
		return parts{}, fmt.Errorf("%s: services.%s.image: %w", cp.file, printableName(name), err)
	}
	driver := imageDriver(image)
	if driver == "" {
		return parts{}, fmt.Errorf("%s: service %s runs no MySQL, MariaDB, PostgreSQL or SQL Server image", cp.file, printableName(name))
	}
	if driver == SQLServer {
		return cp.sqlServerDatabase(root, name, env)
	}
	e, err := cp.environment(root, name, env)
	if err != nil {
		if strings.HasPrefix(err.Error(), "services.") {
			return parts{}, fmt.Errorf("%s: %w", cp.file, err)
		}
		return parts{}, err
	}
	get := func(keys ...string) (string, bool) {
		for _, k := range keys {
			if v, ok := e[k]; ok {
				return v, true
			}
		}
		return "", false
	}
	p := parts{driver: driver}
	target := "3306"
	if driver == Postgres {
		target = "5432"
		p.user = "postgres"
		if v, ok := get("POSTGRES_USER"); ok && v != "" {
			p.user = v
		}
		p.password, _ = get("POSTGRES_PASSWORD")
		p.name = p.user
		if v, ok := get("POSTGRES_DB"); ok && v != "" {
			p.name = v
		}
		if _, ok := get("POSTGRES_PASSWORD_FILE"); ok && p.password == "" {
			return parts{}, fmt.Errorf("%s: service %s takes its password from a file (POSTGRES_PASSWORD_FILE); DriftGate does not read it", cp.file, printableName(name))
		}
	} else {
		p.name, _ = get("MYSQL_DATABASE", "MARIADB_DATABASE")
		rootPw, hasRoot := get("MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD")
		empty, _ := get("MYSQL_ALLOW_EMPTY_PASSWORD", "MARIADB_ALLOW_EMPTY_ROOT_PASSWORD", "ALLOW_EMPTY_PASSWORD")
		switch {
		case hasRoot && rootPw != "":
			p.user, p.password = "root", rootPw
		case empty != "" && empty != "no" && empty != "false" && empty != "0":
			p.user = "root"
		default:
			p.user, _ = get("MYSQL_USER", "MARIADB_USER")
			p.password, _ = get("MYSQL_PASSWORD", "MARIADB_PASSWORD")
		}
		if p.user == "" {
			return parts{}, fmt.Errorf("%s: service %s sets no MYSQL_ROOT_PASSWORD or MYSQL_USER (a password file or a random one is not read)", cp.file, printableName(name))
		}
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
