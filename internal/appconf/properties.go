package appconf

import (
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// parseProperties reads a Java .properties file by the rules of
// java.util.Properties.load: # and ! comment lines, a key ending at the
// first unescaped =, : or space, a \ at the end of a line continuing it,
// and the escapes \t \n \r \f \uXXXX (any other \x is x).
func parseProperties(src []byte) map[string]string {
	out := map[string]string{}
	lines := strings.Split(strings.TrimPrefix(string(src), "\uFEFF"), "\n")
	for n := 0; n < len(lines); n++ {
		l := strings.TrimLeft(strings.TrimRight(lines[n], "\r"), " \t\f")
		if l == "" || l[0] == '#' || l[0] == '!' {
			continue
		}
		for continued(l) && n+1 < len(lines) {
			n++
			l = l[:len(l)-1] + strings.TrimLeft(strings.TrimRight(lines[n], "\r"), " \t\f")
		}
		if continued(l) {
			l = l[:len(l)-1]
		}
		i := 0
		for i < len(l) && l[i] != '=' && l[i] != ':' && l[i] != ' ' && l[i] != '\t' && l[i] != '\f' {
			if l[i] == '\\' {
				i++
			}
			i++
		}
		i = min(i, len(l))
		key := unescapeProperty(l[:i])
		rest := strings.TrimLeft(l[i:], " \t\f")
		if rest != "" && (rest[0] == '=' || rest[0] == ':') {
			rest = strings.TrimLeft(rest[1:], " \t\f")
		}
		out[key] = unescapeProperty(rest)
	}
	return out
}

// continued reports a line that ends with an odd number of backslashes.
func continued(l string) bool {
	n := 0
	for i := len(l) - 1; i >= 0 && l[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

func unescapeProperty(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 'f':
			b.WriteByte('\f')
		case 'u':
			if i+4 < len(s) {
				if r, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
					b.WriteRune(rune(r))
					i += 4
					continue
				}
			}
			b.WriteByte('u')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// flattenYAML reads a YAML file into dotted keys, as Spring and Micronaut
// see it (a.b.c, a.list[0]). Of several documents, one that a profile
// activates (spring.config.activate.on-profile, spring.profiles) is read
// only when that profile is one of profiles; they are merged in order.
func flattenYAML(src []byte, profiles []string) (map[string]string, error) {
	out := map[string]string{}
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	for {
		var doc any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, errors.New("not valid YAML (the content is not shown)")
		}
		m := map[string]string{}
		flatten(doc, "", m)
		on := m["spring.config.activate.on-profile"]
		if on == "" {
			on = m["spring.profiles"]
		}
		if on != "" && !profileActive(on, profiles) {
			continue
		}
		for k, v := range m {
			out[k] = v
		}
	}
	return out, nil
}

func flatten(v any, prefix string, out map[string]string) {
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			flatten(c, join(k), out)
		}
	case map[any]any:
		for k, c := range t {
			flatten(c, join(fmt.Sprint(k)), out)
		}
	case []any:
		for i, c := range t {
			flatten(c, prefix+"["+strconv.Itoa(i)+"]", out)
		}
	case nil:
		out[prefix] = ""
	default:
		out[prefix] = fmt.Sprint(t)
	}
}

// propertyPrefixes are the prefixes of a datasource: Spring, Quarkus,
// Micronaut.
var propertyPrefixes = []string{"spring.datasource", "quarkus.datasource", "datasources.default", "spring.r2dbc"}

// profileActive reports whether a document's on-profile matches: one of
// its names (a, b) is active, or a !name is not. Expressions with & and |
// are not read, so such a document is left out.
func profileActive(on string, profiles []string) bool {
	if strings.ContainsAny(on, "&|()") {
		return false
	}
	for _, n := range strings.Split(on, ",") {
		n = strings.TrimSpace(n)
		if not, ok := strings.CutPrefix(n, "!"); ok {
			if !slices.Contains(profiles, not) {
				return true
			}
		} else if slices.Contains(profiles, n) {
			return true
		}
	}
	return false
}

// readPropertyFile reads one application*.properties or .yml file, with
// the documents of profiles.
func readPropertyFile(root, file string, profiles []string) (map[string]string, error) {
	raw, err := readFile(root, file)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(file, ".properties") {
		return propertyDocuments(raw, profiles), nil
	}
	m, err := flattenYAML(raw, profiles)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return m, nil
}

// springFiles reads, as Spring Boot does, the files of stem in dir:
// .yaml, .yml, then .properties, which wins.
func springFiles(root, dir, stem string, profiles []string, into map[string]string) error {
	for _, ext := range []string{".yaml", ".yml", ".properties"} {
		m, err := readPropertyFile(root, dir+stem+ext, profiles)
		if err != nil {
			if strings.HasSuffix(err.Error(), ": not found") {
				continue
			}
			return err
		}
		for k, v := range m {
			into[k] = v
		}
	}
	return nil
}

// springActive lists the active profiles of the application.* files in
// dir: SPRING_PROFILES_ACTIVE, else their spring.profiles.active.
func springActive(root, dir string, env Lookup) ([]string, error) {
	active, ok := env("SPRING_PROFILES_ACTIVE")
	if !ok {
		base := map[string]string{}
		if err := springFiles(root, dir, "application", nil, base); err != nil {
			return nil, err
		}
		var err error
		if active, err = placeholders(base["spring.profiles.active"], base, env); err != nil {
			return nil, fmt.Errorf("spring.profiles.active: %w", err)
		}
	}
	var profiles []string
	for _, p := range strings.Split(active, ",") {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(profiles, p) {
			profiles = append(profiles, p)
		}
	}
	return profiles, nil
}

// propertyDocuments reads a .properties file of several documents, split
// by a line #--- or !--- (Spring Boot 2.4 and later): a document with
// spring.config.activate.on-profile counts only when that profile is one
// of profiles; they are merged in order.
func propertyDocuments(src []byte, profiles []string) map[string]string {
	out := map[string]string{}
	for _, doc := range splitPropertyDocuments(string(src)) {
		m := parseProperties([]byte(doc))
		if on := m["spring.config.activate.on-profile"]; on != "" && !profileActive(on, profiles) {
			continue
		}
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func splitPropertyDocuments(s string) []string {
	var docs []string
	start := 0
	for i := 0; i <= len(s); {
		end := strings.IndexByte(s[i:], '\n')
		if end < 0 {
			end = len(s) - i
		}
		line := strings.TrimSpace(s[i : i+end])
		if line == "#---" || line == "!---" {
			docs = append(docs, s[start:i])
			start = i + end + 1
		}
		i += end + 1
	}
	if start <= len(s) {
		docs = append(docs, s[start:])
	}
	return docs
}

// springProperties is the configuration Spring Boot builds around file:
// application.* in its folder, then application-<profile>.* for each active
// profile (SPRING_PROFILES_ACTIVE, else spring.profiles.active of the base
// files), the profile of file itself last. A file not named application*
// is read alone.
func springProperties(root, file string, env Lookup) (map[string]string, error) {
	f := strings.ReplaceAll(file, `\`, "/")
	dir, name := path.Split(f)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if _, err := readFile(root, file); err != nil {
		return nil, err
	}
	if stem != "application" && !strings.HasPrefix(stem, "application-") || (ext != ".properties" && ext != ".yml" && ext != ".yaml") {
		return readPropertyFile(root, file, nil)
	}
	profiles, err := springActive(root, dir, env)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if own, ok := strings.CutPrefix(stem, "application-"); ok {
		profiles = append(slices.DeleteFunc(profiles, func(p string) bool { return p == own }), own)
	}
	m := map[string]string{}
	if err := springFiles(root, dir, "application", profiles, m); err != nil {
		return nil, err
	}
	for _, p := range profiles {
		if err := springFiles(root, dir, "application-"+p, profiles, m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// readProperties reads a datasource of a Spring, Quarkus or Micronaut
// configuration (see springProperties): under the prefix key (default: the
// first of propertyPrefixes it sets), its url (a JDBC URL), username,
// password and driver. Quarkus' %dev. keys win over the others.
// ${NAME:default} takes the properties, then the environment.
func readProperties(root, file, key string, env Lookup) (parts, error) {
	m, err := springProperties(root, file, env)
	if err != nil {
		return parts{}, err
	}
	for k, v := range m { // Quarkus' dev profile: what quarkus dev runs with
		if rest, ok := strings.CutPrefix(k, "%dev."); ok {
			m[rest] = v
		}
	}
	if key == "" {
		for _, pre := range propertyPrefixes {
			if datasourceSet(m, pre) {
				key = pre
				break
			}
		}
		if key == "" {
			return parts{}, fmt.Errorf("%s: no %s", file, strings.Join(propertyPrefixes, ", "))
		}
	}
	f := fields{}
	for k, v := range m {
		rest, ok := strings.CutPrefix(k, key+".")
		if !ok || strings.Contains(strings.TrimPrefix(strings.TrimPrefix(rest, "jdbc."), "reactive."), ".") {
			continue
		}
		v, err := placeholders(v, m, env)
		if err != nil {
			return parts{}, fmt.Errorf("%s: %s: %w", file, k, err)
		}
		if rest == "jdbc.url" || rest == "reactive.url" {
			rest = "url"
		}
		f[fieldKey(rest)] = v
	}
	if len(f) == 0 {
		return parts{}, fmt.Errorf("%s: %s is not set", file, key)
	}
	return fromFields(f)
}

// datasourceSet reports whether m sets a datasource under pre.
func datasourceSet(m map[string]string, pre string) bool {
	for _, k := range []string{".url", ".jdbc-url", ".jdbc.url", ".reactive.url", ".username", ".db-kind"} {
		if _, ok := m[pre+k]; ok {
			return true
		}
	}
	return false
}

// placeholders resolves Spring's ${NAME} and ${NAME:default} as Spring
// does: against the properties, then the environment, the name also as
// Spring's relaxed binding writes it there (spring.datasource.password:
// SPRING_DATASOURCE_PASSWORD), then the default.
func placeholders(v string, props map[string]string, env Lookup) (string, error) {
	return resolvePlaceholders(v, props, env, 0)
}

func resolvePlaceholders(v string, props map[string]string, env Lookup, depth int) (string, error) {
	if depth > 16 {
		return "", errors.New("placeholders nest too deep or refer to each other")
	}
	if !strings.Contains(v, "${") {
		return v, nil
	}
	var b strings.Builder
	for {
		i := strings.Index(v, "${")
		if i < 0 {
			b.WriteString(v)
			return b.String(), nil
		}
		b.WriteString(v[:i])
		open, end := 0, -1
		for j := i; j < len(v); j++ {
			switch {
			case strings.HasPrefix(v[j:], "${"):
				open++
				j++
			case v[j] == '}':
				open--
				if open == 0 {
					end = j
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			return "", errors.New("${ without }")
		}
		inner := v[i+2 : end]
		name, def, hasDef := strings.Cut(inner, ":")
		val, ok := props[name]
		if ok {
			var err error
			if val, err = resolvePlaceholders(val, props, env, depth+1); err != nil {
				return "", err
			}
		} else {
			val, ok = env(name)
		}
		if !ok {
			val, ok = env(strings.ToUpper(strings.NewReplacer(".", "_", "-", "").Replace(name)))
		}
		if !ok {
			if !hasDef {
				return "", fmt.Errorf("${%s} is not set in the properties or the environment", printableName(name))
			}
			var err error
			if val, err = resolvePlaceholders(def, props, env, depth+1); err != nil {
				return "", err
			}
		}
		b.WriteString(val)
		v = v[end+1:]
	}
}

// printableName is a variable name read from a file, safe to quote in an
// error: a name, not a value.
func printableName(s string) string {
	if s == "" || len(s) > 80 || strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-") != "" {
		return "…"
	}
	return s
}
