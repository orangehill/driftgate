package appconf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// goos is runtime.GOOS; a test sets windows.
var goos = runtime.GOOS

// stripJSONC turns JSON as ASP.NET reads it (// and /* */ comments,
// trailing commas, a byte order mark) into JSON.
func stripJSONC(src []byte) []byte {
	src = []byte(strings.TrimPrefix(string(src), "\uFEFF"))
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j, len(src)-1)
			out = append(out, src[i:j+1]...)
			i = j
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			i--
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(string(src[i+2:]), "*/")
			if end < 0 {
				return out
			}
			i += end + 3
		case c == '}' || c == ']':
			// A comma before it, past spaces, is a trailing one.
			k := len(out) - 1
			for k >= 0 && (out[k] == ' ' || out[k] == '\t' || out[k] == '\r' || out[k] == '\n') {
				k--
			}
			if k >= 0 && out[k] == ',' {
				out = append(out[:k], out[k+1:]...)
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// settings are configuration keys as ASP.NET names them, joined with :
// (ConnectionStrings:Default), array items by index; by the key in lower
// case, as they are case-insensitive, with the key as written and the
// value.
type settings map[string][2]string

// readJSONSettings reads a JSON settings file into its configuration keys.
func readJSONSettings(root, file string, abs bool, into settings) error {
	var raw []byte
	var err error
	if abs {
		raw, err = os.ReadFile(file)
		if err != nil {
			return errors.New("user secrets: cannot read secrets.json")
		}
	} else if raw, err = readFile(root, file); err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(stripJSONC(raw), &v); err != nil {
		if abs {
			return errors.New("user secrets: secrets.json is not valid JSON")
		}
		return fmt.Errorf("%s: not valid JSON (the content is not shown)", file)
	}
	flattenJSON(v, "", into)
	return nil
}

func flattenJSON(v any, prefix string, out settings) {
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + ":" + k
	}
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			flattenJSON(c, join(k), out)
		}
	case []any:
		for i, c := range t {
			flattenJSON(c, join(strconv.Itoa(i)), out)
		}
	case nil:
		out[strings.ToLower(prefix)] = [2]string{prefix, ""}
	case string:
		out[strings.ToLower(prefix)] = [2]string{prefix, t}
	default:
		out[strings.ToLower(prefix)] = [2]string{prefix, fmt.Sprint(t)}
	}
}

// aspnetEnvironment is the environment ASP.NET runs in on this machine:
// ASPNETCORE_ENVIRONMENT, DOTNET_ENVIRONMENT, else Development (what dotnet
// run sets through launchSettings.json).
func aspnetEnvironment(env Lookup) string {
	for _, n := range []string{"ASPNETCORE_ENVIRONMENT", "DOTNET_ENVIRONMENT"} {
		if v, ok := env(n); ok && v != "" {
			return v
		}
	}
	return "Development"
}

// appSettingsFiles lists the files ASP.NET reads for file, in order: for a
// .csproj, appsettings.json and appsettings.<environment>.json in its
// folder (and its user secrets, see userSecrets); for
// appsettings.<environment>.json, appsettings.json under it; else file.
func appSettingsFiles(file string, env Lookup) []string {
	f := strings.ReplaceAll(file, `\`, "/")
	dir, name := path.Split(f)
	switch {
	case strings.HasSuffix(name, ".csproj"):
		return []string{dir + "appsettings.json", dir + "appsettings." + aspnetEnvironment(env) + ".json"}
	case strings.EqualFold(name, "appsettings.json"):
		return []string{f}
	case strings.HasPrefix(strings.ToLower(name), "appsettings."):
		return []string{dir + "appsettings.json", f}
	}
	return []string{f}
}

// userSecretsID reads <UserSecretsId> of a project file; an id that could
// name another folder is refused.
func userSecretsID(csproj string) (string, error) {
	_, rest, ok := strings.Cut(csproj, "<UserSecretsId>")
	if !ok {
		return "", nil
	}
	id, _, ok := strings.Cut(rest, "</UserSecretsId>")
	id = strings.TrimSpace(id)
	if !ok || id == "" || strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" || strings.Contains(id, "..") {
		return "", errors.New("UserSecretsId is not a plain id")
	}
	return id, nil
}

// userSecretsPath is where dotnet user-secrets keeps the secrets of id:
// %APPDATA%\Microsoft\UserSecrets\<id>\secrets.json on Windows,
// ~/.microsoft/usersecrets/<id>/secrets.json elsewhere.
func userSecretsPath(id string, env Lookup) string {
	if goos == "windows" {
		if appdata, ok := env("APPDATA"); ok && appdata != "" {
			return strings.Join([]string{strings.TrimRight(appdata, `\/`), "Microsoft", "UserSecrets", id, "secrets.json"}, `\`)
		}
		return ""
	}
	home, ok := env("HOME")
	if !ok || home == "" {
		return ""
	}
	return filepath.Join(home, ".microsoft", "usersecrets", id, "secrets.json")
}

// appSettings reads the configuration ASP.NET builds from file (see
// appSettingsFiles) and, for a .csproj in Development, its user secrets,
// which are only read.
func appSettings(root, file string, env Lookup) (settings, error) {
	m := settings{}
	var launch map[string]string
	if strings.HasSuffix(file, ".csproj") {
		// dotnet run sets the variables of its launch profile.
		lp := path.Join(path.Dir(strings.ReplaceAll(file, `\`, "/")), "Properties/launchSettings.json")
		if raw, err := readFile(root, lp); err == nil {
			_, launch = launchProfile(raw)
		}
		if len(launch) > 0 {
			outer := env
			env = func(name string) (string, bool) {
				if v, ok := launch[name]; ok {
					return v, true
				}
				return outer(name)
			}
		}
	}
	files := appSettingsFiles(file, env)
	read := 0
	for _, f := range files {
		if err := readJSONSettings(root, f, false, m); err != nil {
			if strings.HasSuffix(err.Error(), ": not found") && len(files) > 1 {
				continue
			}
			return nil, err
		}
		read++
	}
	if strings.HasSuffix(file, ".csproj") {
		raw, err := readFile(root, file)
		if err != nil {
			return nil, err
		}
		id, err := userSecretsID(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if id != "" && aspnetEnvironment(env) == "Development" {
			if p := userSecretsPath(id, env); p != "" {
				if _, err := os.Stat(p); err == nil {
					if err := readJSONSettings("", p, true, m); err != nil {
						return nil, fmt.Errorf("%s: %w", file, err)
					}
					read++
				}
			}
		}
	}
	if read == 0 {
		return nil, fmt.Errorf("%s: no appsettings.json next to it", file)
	}
	for k, v := range launch { // variables come after the files
		if strings.HasPrefix(k, "ConnectionStrings__") {
			key := strings.ReplaceAll(k, "__", ":")
			m[strings.ToLower(key)] = [2]string{key, v}
		}
	}
	return m, nil
}

// readAppSettings reads the connection string at key (ConnectionStrings:
// Default; case does not matter) of an ASP.NET configuration.
func readAppSettings(root, file, key string, env Lookup) (parts, error) {
	if key == "" {
		return parts{}, fmt.Errorf("%s: needs key (e.g. ConnectionStrings:Default)", file)
	}
	m, err := appSettings(root, file, env)
	if err != nil {
		return parts{}, err
	}
	v := m[strings.ToLower(strings.ReplaceAll(key, "__", ":"))][1]
	if strings.TrimSpace(v) == "" {
		return parts{}, fmt.Errorf("%s: %s is not set", file, key)
	}
	return fromValue(v)
}

// csprojDriver is the database a project's packages use: mysql, postgres,
// or another (SQL Server, SQLite); empty when none says.
func csprojDriver(csproj string) (driver, other string) {
	lc := strings.ToLower(csproj)
	switch {
	case strings.Contains(lc, `"pomelo.entityframeworkcore.mysql`), strings.Contains(lc, `"mysqlconnector`), strings.Contains(lc, `"mysql.data`), strings.Contains(lc, `"mysql.entityframeworkcore`):
		return MySQL, ""
	case strings.Contains(lc, `"npgsql`):
		return Postgres, ""
	case strings.Contains(lc, `"microsoft.entityframeworkcore.sqlserver`), strings.Contains(lc, `"microsoft.data.sqlclient`), strings.Contains(lc, `"system.data.sqlclient`):
		return "", "SQL Server"
	case strings.Contains(lc, `"microsoft.entityframeworkcore.sqlite`), strings.Contains(lc, `"microsoft.data.sqlite`):
		return "", "SQLite"
	}
	return "", ""
}

// launchProfile reads the profile of a launchSettings.json that dotnet run
// takes: the first with commandName Project, else the first. It returns
// its addresses (applicationUrl, separated by ;) and its
// environmentVariables.
func launchProfile(raw []byte) (urls []string, env map[string]string) {
	var top struct {
		Profiles json.RawMessage `json:"profiles"`
	}
	if json.Unmarshal(stripJSONC(raw), &top) != nil || len(top.Profiles) == 0 {
		return nil, nil
	}
	type profile struct {
		CommandName          string            `json:"commandName"`
		ApplicationURL       string            `json:"applicationUrl"`
		EnvironmentVariables map[string]string `json:"environmentVariables"`
	}
	dec := json.NewDecoder(bytes.NewReader(top.Profiles))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil
	}
	var chosen *profile
	for dec.More() {
		if _, err := dec.Token(); err != nil { // the name
			return nil, nil
		}
		var p profile
		if dec.Decode(&p) != nil {
			return nil, nil
		}
		if chosen == nil || p.CommandName == "Project" && chosen.CommandName != "Project" {
			chosen = &p
		}
	}
	if chosen == nil {
		return nil, nil
	}
	for _, u := range strings.Split(chosen.ApplicationURL, ";") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	return urls, chosen.EnvironmentVariables
}
