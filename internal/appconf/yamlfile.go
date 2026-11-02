package appconf

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// computed marks, in a YAML file read through erb, where an ERB tag holds
// Ruby that DriftGate does not run.
const computed = "__driftgate_computed__"

// erb replaces the ERB tags of a Rails YAML file with what they print, for
// the forms that read the environment: ENV['X'], ENV["X"], ENV.fetch('X'),
// ENV.fetch('X', default), ENV.fetch('X') { default } and ENV['X'] ||
// default, a default being a quoted string or a number. Any other output
// tag becomes computed; tags that print nothing (<% … %>, <%# … %>) are
// dropped.
func erb(s string, env Lookup) string {
	if !strings.Contains(s, "<%") {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, "<%")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		end := strings.Index(s[i+2:], "%>")
		if end < 0 {
			b.WriteString(computed)
			return b.String()
		}
		tag := s[i+2 : i+2+end]
		s = s[i+2+end+2:]
		if !strings.HasPrefix(tag, "=") {
			continue
		}
		v, ok := rubyEnv(strings.TrimSpace(strings.TrimSuffix(tag[1:], "-")), env)
		if !ok {
			v = computed
		}
		b.WriteString(v)
	}
}

// rubyEnv evaluates the ENV forms erb takes.
func rubyEnv(e string, env Lookup) (string, bool) {
	var name, def string
	switch {
	case strings.HasPrefix(e, "ENV["):
		n, rest, ok := rubyString(e[4:])
		if !ok || !strings.HasPrefix(rest, "]") {
			return "", false
		}
		name, e = n, strings.TrimSpace(rest[1:])
		if after, ok := strings.CutPrefix(e, "||"); ok {
			if def, e, ok = rubyLiteral(strings.TrimSpace(after)); !ok {
				return "", false
			}
		}
	case strings.HasPrefix(e, "ENV.fetch("):
		n, rest, ok := rubyString(e[len("ENV.fetch("):])
		if !ok {
			return "", false
		}
		name, rest = n, strings.TrimSpace(rest)
		if after, ok := strings.CutPrefix(rest, ","); ok {
			if def, rest, ok = rubyLiteral(strings.TrimSpace(after)); !ok {
				return "", false
			}
			rest = strings.TrimSpace(rest)
		}
		if !strings.HasPrefix(rest, ")") {
			return "", false
		}
		e = strings.TrimSpace(rest[1:])
		if after, ok := strings.CutPrefix(e, "{"); ok {
			after = strings.TrimSpace(after)
			if strings.HasPrefix(after, "|") { // { |name| 'default' }
				if j := strings.IndexByte(after[1:], '|'); j >= 0 {
					after = strings.TrimSpace(after[j+2:])
				}
			}
			if def, e, ok = rubyLiteral(after); !ok {
				return "", false
			}
			e = strings.TrimPrefix(strings.TrimSpace(e), "}")
		}
	default:
		return "", false
	}
	if strings.TrimSpace(e) != "" {
		return "", false
	}
	if v, ok := env(name); ok { // an empty value is a value in Ruby
		return v, true
	}
	return def, true
}

// rubyString reads a quoted string at the start of s.
func rubyString(s string) (v, rest string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '\'' && s[0] != '"') {
		return "", "", false
	}
	end := strings.IndexByte(s[1:], s[0])
	if end < 0 {
		return "", "", false
	}
	return s[1 : 1+end], s[end+2:], true
}

// rubyLiteral reads a quoted string or a number at the start of s.
func rubyLiteral(s string) (v, rest string, ok bool) {
	if v, rest, ok := rubyString(s); ok {
		return v, rest, true
	}
	i := 0
	for i < len(s) && ('0' <= s[i] && s[i] <= '9' || s[i] == '.' || i == 0 && s[i] == '-') {
		i++
	}
	if i == 0 {
		return "", "", false
	}
	return s[:i], s[i:], true
}

// yamlKeys are the keys a Go service's config.yaml usually holds its
// database under.
var yamlKeys = []string{"database", "db", "datasource", "mysql", "postgres", "postgresql"}

// readYAML reads a database setting of a YAML file at key, a path of keys
// separated by dots (development for Rails' config/database.yml,
// development.primary for one of several databases, database for a Go
// config.yaml; default: development for a database.yml, else the first of
// yamlKeys the file has). The setting is a URL, or a map with adapter (or
// driver), host, port, database (or name, dbname), username (or user),
// password, socket and url. ERB tags are read as erb says.
func readYAML(root, file, key string, env Lookup) (parts, error) {
	raw, err := readFile(root, file)
	if err != nil {
		return parts{}, err
	}
	var doc any
	if err := yaml.Unmarshal([]byte(erb(string(raw), env)), &doc); err != nil {
		return parts{}, fmt.Errorf("%s: not valid YAML (the content is not shown)", file)
	}
	doc = yamlMap(doc)
	if key == "" {
		if railsDatabaseYML(file) {
			key = "development"
		} else {
			for _, k := range yamlKeys {
				if _, ok := at(doc, k); ok {
					key = k
					break
				}
			}
		}
		if key == "" {
			return parts{}, fmt.Errorf("%s: no %s (name the key)", file, strings.Join(yamlKeys, ", "))
		}
	}
	node, ok := at(doc, key)
	if !ok {
		return parts{}, fmt.Errorf("%s: %s is not set", file, key)
	}
	f := fields{}
	switch t := node.(type) {
	case string:
		f["url"] = t
	case map[string]any:
		for k, v := range t {
			switch v := v.(type) {
			case string:
				f[fieldKey(k)] = v
			case int:
				f[fieldKey(k)] = strconv.Itoa(v)
			case nil:
			case map[string]any, []any:
			default:
				f[fieldKey(k)] = fmt.Sprint(v)
			}
		}
	default:
		return parts{}, fmt.Errorf("%s: %s is not a database setting", file, key)
	}
	for k, v := range f {
		if strings.Contains(v, computed) {
			switch k {
			case "url", "host", "port", "database", "dbname", "name", "username", "user", "password", "adapter", "socket":
				return parts{}, fmt.Errorf("%s: %s.%s is computed by Ruby code in an ERB tag; DriftGate does not run it (put the DSN in an environment variable and use dsn: { env: NAME })", file, key, printableName(k))
			}
			delete(f, k)
		}
	}
	p, err := fromFields(f)
	if err != nil || !railsDatabaseYML(file) {
		return p, err
	}
	// Rails merges DATABASE_URL over the configuration of the environment
	// it runs in.
	if u, ok := env("DATABASE_URL"); ok && strings.TrimSpace(u) != "" {
		up, err := fromValue(u)
		if err != nil {
			return parts{}, fmt.Errorf("environment variable DATABASE_URL: %w", err)
		}
		up.merge(p)
		return up, nil
	}
	return p, nil
}

func railsDatabaseYML(file string) bool {
	return strings.HasSuffix(file, "database.yml") || strings.HasSuffix(file, "database.yaml")
}

// yamlMap turns the maps yaml.v3 decodes (map[string]any, and map[any]any
// for keys that are not strings) into map[string]any all the way down, so
// at can follow them.
func yamlMap(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			t[k] = yamlMap(c)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, c := range t {
			m[fmt.Sprint(k)] = yamlMap(c)
		}
		return m
	}
	return v
}
