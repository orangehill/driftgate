package appconf

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseDotenv(t *testing.T) {
	env := lookup(map[string]string{"HOME": "/home/me", "EMPTY": ""})
	tests := []struct {
		name, src string
		want      map[string]string
		err       string
	}{
		{"plain", "A=1\nB=two words\n", map[string]string{"A": "1", "B": "two words"}, ""},
		{"spaces around =", "A = 1\n  B= 2 \t\n", map[string]string{"A": "1", "B": "2"}, ""},
		{"export", "export A=1\nexport\tB='x'\n", map[string]string{"A": "1", "B": "x"}, ""},
		{"comments", "# c\nA=1 # inline\nB=a#b\nC=\"x # y\" # z\n  # indented\n", map[string]string{"A": "1", "B": "a#b", "C": "x # y"}, ""},
		{"empty values", "A=\nB=''\nC=\"\"\n", map[string]string{"A": "", "B": "", "C": ""}, ""},
		{"single quotes are literal", `A='a\nb ${HOME} $x "q"'`, map[string]string{"A": `a\nb ${HOME} $x "q"`}, ""},
		{"backticks", "A=`it's \"x\"`", map[string]string{"A": `it's "x"`}, ""},
		{"double quote escapes", `A="a\nb\tc\"d\\e\$f\qg"`, map[string]string{"A": "a\nb\tc\"d\\e$f\\qg"}, ""},
		{"multiline double", "A=\"line1\nline2\"\nB=3", map[string]string{"A": "line1\nline2", "B": "3"}, ""},
		{"multiline single", "KEY='-----BEGIN-----\nabc\n-----END-----'\nB=3", map[string]string{"KEY": "-----BEGIN-----\nabc\n-----END-----", "B": "3"}, ""},
		{"expansion", "A=x\nB=${A}-${HOME}\nC=\"${A}/${MISSING}\"\n", map[string]string{"A": "x", "B": "x-/home/me", "C": "x/"}, ""},
		{"defaults", "A=${MISSING:-d1}\nB=${EMPTY:-d2}\nC=${EMPTY-d3}\nD=${MISSING-d4}\n", map[string]string{"A": "d1", "B": "d2", "C": "", "D": "d4"}, ""},
		{"file before environment", "HOME=/srv\nA=${HOME}\n", map[string]string{"HOME": "/srv", "A": "/srv"}, ""},
		{"dollar without braces is kept", "A=pa$word\nB=\"$$x\"\n", map[string]string{"A": "pa$word", "B": "$$x"}, ""},
		{"escaped dollar", `A="\${HOME}"`, map[string]string{"A": "${HOME}"}, ""},
		{"crlf", "A=1\r\nB=\"2\"\r\n", map[string]string{"A": "1", "B": "2"}, ""},
		{"bom", "\uFEFFA=1", map[string]string{"A": "1"}, ""},
		{"dots and dashes in names", "spring.datasource.url=x\nmy-key=y", map[string]string{"spring.datasource.url": "x", "my-key": "y"}, ""},
		{"lines without = are skipped", "just text\nA=1\n=x\n", map[string]string{"A": "1"}, ""},
		{"later wins", "A=1\nA=2", map[string]string{"A": "2"}, ""},
		{"quote with text after", "A=\"x\"y\nB=1", map[string]string{"A": "x", "B": "1"}, ""},
		{"laravel null", "REDIS_PASSWORD=null", map[string]string{"REDIS_PASSWORD": "null"}, ""},
		{"unterminated double", "A=1\nB=\"abc\nC=2", nil, `line 2: the value of B has no closing "`},
		{"unterminated single", "B='abc", nil, "line 1: the value of B has no closing '"},
		{"brace without end is literal", "B=pa${ss\nC=\"pa${ss\"\n", map[string]string{"B": "pa${ss", "C": "pa${ss"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDotenv([]byte(tt.src), env)
			if tt.err != "" {
				if err == nil || err.Error() != tt.err {
					t.Fatalf("error %v, want %s", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestParseConnString(t *testing.T) {
	tests := []struct {
		src  string
		want fields
		err  string
	}{
		{"Server=localhost;Port=3306;Database=shop;Uid=shop;Pwd=pw;", fields{"server": "localhost", "port": "3306", "database": "shop", "uid": "shop", "pwd": "pw"}, ""},
		{" User Id = a b ; Password = x ", fields{"userid": "a b", "password": "x"}, ""},
		{"Password='a;b''c';Host=h", fields{"password": "a;b'c", "host": "h"}, ""},
		{`Password="a;""b"" ";Host=h`, fields{"password": `a;"b" `, "host": "h"}, ""},
		{"Pass==word=x", fields{"pass=word": "x"}, ""},
		{"Password=a=b", fields{"password": "a=b"}, ""},
		{"A=1;;;B=2;", fields{"a": "1", "b": "2"}, ""},
		{"A=1;A=2", fields{"a": "2"}, ""},
		{"", fields{}, ""},
		{"Server", nil, "connection string: pair 1 has no ="},
		{"A=1;B", nil, "connection string: pair 2 has no ="},
		{"=x", nil, "connection string: pair 1 has no key"},
		{"Password='abc", nil, "connection string: the value of pair 1 has no closing quote"},
		{"Password='a'b;", nil, "connection string: text after the quoted value of pair 1"},
	}
	for _, tt := range tests {
		got, err := parseConnString(tt.src)
		if tt.err != "" {
			if err == nil || err.Error() != tt.err {
				t.Errorf("%q: error %v, want %s", tt.src, err, tt.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %q, %v; want %q", tt.src, got, err, tt.want)
		}
	}
}

func TestFromValue(t *testing.T) {
	tests := []struct {
		value, driver, dsn, err string
	}{
		// URLs as applications hold them.
		{"mysql://u:p@db:3307/app?charset=utf8mb4&serverVersion=8.0", "", "mysql://u:p@db:3307/", ""},
		{"mysql2://u:p@db/app", MySQL, "mysql://u:p@db:3306/", ""},
		{"mariadb://u@db/app", "mariadb", "mysql://u@db:3306/", ""},
		{"pdo-mysql://u:p@db/app", "", "mysql://u:p@db:3306/", ""},
		{"mysql+pymysql://u:p@db/app?ssl_mode=VERIFY_IDENTITY", "", "mysql://u:p@db:3306/?ssl-mode=VERIFY_IDENTITY", ""},
		{"trilogy://u:p@db/app", "", "mysql://u:p@db:3306/", ""},
		{"postgresql://u:p@db/app?sslmode=require&schema=public&connection_limit=5", "", "postgres://u:p@db:5432/?sslmode=require", ""},
		{"pgsql://u:p@db:5433/app", Postgres, "postgres://u:p@db:5433/", ""},
		{"postgis://u:p@db/app", "", "postgres://u:p@db:5432/", ""},
		{"postgresql+psycopg2://u:p@db/app", "", "postgres://u:p@db:5432/", ""},
		{"postgres://u:p@[::1]:5432/app", "", "postgres://u:p@[::1]:5432/", ""},
		{"jdbc:mysql://db:3306/app?user=u&password=p%26q&useSSL=false", "", "mysql://u:p&q@db:3306/?ssl-mode=DISABLED", ""},
		{"jdbc:postgresql://db/app?user=u&password=p&ssl=true&sslmode=verify-full", "", "postgres://u:p@db:5432/?sslmode=verify-full", ""},
		{"jdbc:mariadb://db/app", "", "mysql://db:3306/", ""},
		{"vertx-reactive:postgresql://db/app", "", "postgres://db:5432/", ""},
		{"r2dbc:pool:mysql://u:p@db/app", "", "mysql://u:p@db:3306/", ""},
		{"postgres://u:p@/app?host=/var/run/postgresql", "", "postgres://u:p@/?host=%2Fvar%2Frun%2Fpostgresql", ""},
		// The MySQL driver's own form is used as it is.
		{"u:p@tcp(db:3306)/app?parseTime=true", MySQL, "u:p@tcp(db:3306)/app?parseTime=true", ""},
		{"u:p@unix(/tmp/mysql.sock)/app", "", "u:p@unix(/tmp/mysql.sock)/app", ""},
		// ADO.NET.
		{"Server=db;Port=3307;Database=app;Uid=u;Pwd=p", MySQL, "mysql://u:p@db:3307/", ""},
		{"Server=db1,db2;Database=app;User ID=u;Password=p", MySQL, "mysql://u:p@db1:3306/", ""},
		{"Host=db;Database=app;Username=u;Password=p;Pooling=true", Postgres, "postgres://u:p@db:5432/", ""},
		{"Host=/var/run/postgresql;Database=app;Username=u", Postgres, "postgres://u@/?host=%2Fvar%2Frun%2Fpostgresql", ""},
		{"Server=localhost;Protocol=unix;Database=app;Uid=u", MySQL, "mysql://u@localhost:3306/", ""},
		{"Host=db;Database=app;Username=u;Password=p", "", "", "x does not say which database it is; set the driver (mysql or postgres)"},
		{"Server=(localdb)\\mssqllocaldb;Database=x;Trusted_Connection=True", MySQL, "", "x names SQL Server; DriftGate restores MySQL, MariaDB and PostgreSQL"},
		{"Server=tcp:x.database.windows.net,1433;Initial Catalog=x;User ID=u;Password=p", MySQL, "", "x names SQL Server; DriftGate restores MySQL, MariaDB and PostgreSQL"},
		{"Data Source=app.db", MySQL, "", "x names SQLite; DriftGate restores MySQL, MariaDB and PostgreSQL"},
		// Others.
		{"sqlite:///var/data.db", MySQL, "", "x names SQLite; DriftGate restores MySQL, MariaDB and PostgreSQL"},
		{"sqlsrv://u:p@db/app", MySQL, "", "x names SQL Server; DriftGate restores MySQL, MariaDB and PostgreSQL"},
		{"redis://u:p@db", MySQL, "", "a redis:// URL names no database DriftGate knows (mysql, postgres)"},
		{"mysql://u:p@db/app", Postgres, "", "x names a mysql database, the driver is postgres"},
		{"mysql://u:p%zz@db/app", MySQL, "", errURL.Error()},
		{"just text", MySQL, "", "not a database URL, a MySQL DSN or a connection string (the value is not shown)"},
	}
	for _, tt := range tests {
		p, err := fromValue(tt.value)
		var dsn string
		if err == nil {
			dsn, err = p.dsn(tt.driver, "x")
		}
		if tt.err != "" {
			if err == nil || err.Error() != tt.err {
				t.Errorf("%s: error %v, want %s", tt.value, err, tt.err)
			}
			continue
		}
		if err != nil || dsn != tt.dsn {
			t.Errorf("%s: %s, %v; want %s", tt.value, dsn, err, tt.dsn)
		}
	}
}

func TestParseProperties(t *testing.T) {
	src := "# comment\n! also\n  a.b = 1\nc:2\nd 3\ne=multi \\\n    line\nf=tab\\tu\\u00e9\\\\\ng\\ key=v\nh=\\\ni=last\\"
	want := map[string]string{"a.b": "1", "c": "2", "d": "3", "e": "multi line", "f": "tab\tué\\", "g key": "v", "h": "i=last"}
	if got := parseProperties([]byte(src)); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestPlaceholders(t *testing.T) {
	env := lookup(map[string]string{"DB_PASSWORD": "p", "SPRING_DATASOURCE_USERNAME": "relaxed"})
	for _, tt := range []struct{ in, out, err string }{
		{"plain", "plain", ""},
		{"${DB_PASSWORD}", "p", ""},
		{"${DB_PASSWORD:def}", "p", ""},
		{"${MISSING:def}", "def", ""},
		{"${MISSING:}", "", ""},
		{"${MISSING:${DB_PASSWORD}}", "p", ""},
		{"a-${MISSING:x:y}-b", "a-x:y-b", ""},
		{"${spring.datasource.username}", "relaxed", ""},
		{"${MISSING}", "", "${MISSING} is not set in the properties or the environment"},
		{"${MISSING", "", "${ without }"},
	} {
		got, err := placeholders(tt.in, nil, env)
		if tt.err != "" {
			if err == nil || err.Error() != tt.err {
				t.Errorf("%s: error %v, want %s", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.out {
			t.Errorf("%s: %q, %v; want %q", tt.in, got, err, tt.out)
		}
	}
}

func TestInterpolate(t *testing.T) {
	vars := lookup(map[string]string{"A": "a", "EMPTY": ""})
	for _, tt := range []struct{ in, out, err string }{
		{"$A ${A} $$A", "a a $A", ""},
		{"${MISSING:-d} ${EMPTY:-d} ${EMPTY-d} ${MISSING-d}", "d d  d", ""},
		{"${A:+set} ${MISSING:+set}|", "set |", ""},
		{"${A:?need} ok", "a ok", ""},
		{"${MISSING:-${A}}", "a", ""},
		{"${FORWARD_DB_PORT:-3306}:3306", "3306:3306", ""},
		{"x$", "x$", ""},
		{"${MISSING:?msg}", "", "variable MISSING is required and not set"},
		{"${EMPTY:?msg}", "", "variable EMPTY is required and not set"},
		{"${A", "", "${ without }"},
	} {
		got, err := interpolate(tt.in, vars)
		if tt.err != "" {
			if err == nil || err.Error() != tt.err {
				t.Errorf("%s: error %v, want %s", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.out {
			t.Errorf("%s: %q, %v; want %q", tt.in, got, err, tt.out)
		}
	}
}

func TestERB(t *testing.T) {
	env := lookup(map[string]string{"SET": "v", "EMPTY": ""})
	for _, tt := range []struct{ in, out string }{
		{`<%= ENV['SET'] %>`, "v"},
		{`<%= ENV["MISSING"] %>`, ""},
		{`<%= ENV.fetch("SET") %>`, "v"},
		{`<%= ENV.fetch("MISSING") { 5 } %>`, "5"},
		{`<%= ENV.fetch("MISSING") { |k| "d" } %>`, "d"},
		{`<%= ENV.fetch('MISSING', 'd') %>`, "d"},
		{`<%= ENV.fetch('EMPTY', 'd') %>`, ""},
		{`<%= ENV['EMPTY'] || 'd' %>`, ""},
		{`<%= ENV['MISSING'] || "d" %>`, "d"},
		{`<%= ENV['SET'] || "d" %>`, "v"},
		{`<%= ENV['SET'] -%>`, "v"},
		{`a<% if true %>b<%# note %>c`, "abc"},
		{`<%= Rails.application.credentials.db %>`, computed},
		{`<%= ENV['SET'].to_i + 1 %>`, computed},
		{`x <%= ENV['SET']`, "x " + computed},
	} {
		if got := erb(tt.in, env); got != tt.out {
			t.Errorf("%s: %q, want %q", tt.in, got, tt.out)
		}
	}
}

func TestLiteral(t *testing.T) {
	php := `<?php
// comment
return array(
    'a' => 'it\'s \n',
    "b" => "x\ty",
    'c' => "interp $x",
    'd' => [1, 2, 'three' => true],
    'e' => null,
    'f' => __DIR__ . '/x',
    'g' => getenv('X'),
    \PDO::ATTR_X => 'k', # constant key
    /* block */ 'h' => -5,
);`
	l := &literal{s: php, i: strings.Index(php, "return") + 6}
	v, err := l.value(0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"a": `it's \n`, "b": "x\ty", "c": code{},
		"d": map[string]any{"0": "1", "1": "2", "three": "true"},
		"e": "", "f": code{}, "g": code{}, "\x00code": "k", "h": "-5",
	}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("PHP:\n got %#v\nwant %#v", v, want)
	}
	py := `DATABASES = {
    "default": {
        'ENGINE': 'django.db.backends.mysql',  # comment
        'NAME': r'C:\db',
        'USER': f'{x}',
        'PASSWORD': os.environ.get('P', 'x'),
        'HOST': None, 'PORT': 3306,
        'TEST': {'NAME': '''t'''},
        'LIST': ['a', ('b', 'c')],
    }
}`
	v, err = djangoDatabases(py)
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"default": map[string]any{
		"ENGINE": "django.db.backends.mysql", "NAME": `C:\db`, "USER": code{}, "PASSWORD": code{}, "HOST": "", "PORT": "3306",
		"TEST": map[string]any{"NAME": "t"}, "LIST": map[string]any{"0": "a", "1": map[string]any{"0": "b", "1": "c"}},
	}}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("Python:\n got %#v\nwant %#v", v, want)
	}
	for _, bad := range []string{"return [", "return ['a' => 'b'", "return ['a' => 'unterminated];", "return " + strings.Repeat("[", 100) + strings.Repeat("]", 100)} {
		l := &literal{s: bad, i: 6}
		if _, err := l.value(0); err == nil {
			t.Errorf("%.30s: no error", bad)
		}
	}
}

func TestStripJSONC(t *testing.T) {
	src := "\uFEFF{\n // c\n \"a\": \"http://x/*y*/\", /* b */\n \"b\": [1, 2, ],\n \"c\": {\"d\": \"\\\"//\",},\n}"
	var got any
	if err := json.Unmarshal(stripJSONC([]byte(src)), &got); err != nil {
		t.Fatalf("%v: %s", err, stripJSONC([]byte(src)))
	}
	want := map[string]any{"a": "http://x/*y*/", "b": []any{1.0, 2.0}, "c": map[string]any{"d": `"//`}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v\nwant %#v", got, want)
	}
}

func FuzzDotenv(f *testing.F) {
	for _, s := range []string{
		"A=1\nB='2'\nC=\"3\\n${A}\"\n", "export A=${B:-x}", "A=\"x\ny\"", "#c\nA=1 # c", "A=`x`", "A=${", "A=\"\\", "\uFEFFA=1\r\n",
	} {
		f.Add(s)
	}
	env := lookup(map[string]string{"HOME": "/h"})
	f.Fuzz(func(t *testing.T, s string) {
		m, err := parseDotenv([]byte(s), env)
		if err != nil {
			// An error names a line and a key, never a value.
			if strings.Contains(s, "SECRETVALUE") && strings.Contains(err.Error(), "SECRETVALUE") && !strings.Contains(s, "SECRETVALUE=") {
				t.Fatalf("error quotes the value: %v", err)
			}
			return
		}
		for k := range m {
			if k == "" || strings.ContainsAny(k, "=\n \t#") {
				t.Fatalf("bad name %q", k)
			}
		}
		// A value written back in single quotes reads the same.
		for k, v := range m {
			if strings.ContainsRune(v, '\'') || !isName(k) {
				continue
			}
			again, err := parseDotenv([]byte(k+"='"+v+"'"), nil)
			if err != nil || again[k] != v {
				t.Fatalf("%q=%q read back as %q, %v", k, v, again[k], err)
			}
		}
	})
}

func isName(k string) bool {
	if strings.HasPrefix(k, "export") {
		return false
	}
	for i := 0; i < len(k); i++ {
		if !isNameByte(k[i]) {
			return false
		}
	}
	return k != ""
}

func FuzzConnString(f *testing.F) {
	for _, s := range []string{
		"Server=h;Port=1;Database=d;Uid=u;Pwd=p", "Password='a;b''c'", `Password="x""y"`, "a==b=c", ";;", "A=1;B", "Host=h;Username=u",
	} {
		f.Add(s, "p@ss;w'o\"rd")
	}
	f.Fuzz(func(t *testing.T, s, password string) {
		if kv, err := parseConnString(s); err == nil {
			p, err := fromConnString(kv)
			if err == nil {
				p.dsn(MySQL, "x")
				p.dsn(Postgres, "x")
			}
		}
		// Any password, quoted as .NET quotes it, reads back the same.
		if strings.ContainsAny(password, "\x00") {
			return
		}
		quoted := "'" + strings.ReplaceAll(password, "'", "''") + "'"
		kv, err := parseConnString("Server=h;Pwd=" + quoted + ";Uid=u")
		if strings.TrimSpace(password) != password || password == "" {
			return // .NET trims an unquoted value; a quoted one keeps them, but the test keeps it simple
		}
		if err != nil || kv["pwd"] != password {
			t.Fatalf("%q read back as %q, %v", password, kv["pwd"], err)
		}
	})
}

func TestConvert(t *testing.T) {
	for _, tt := range []struct{ in, driver, out, err string }{
		{"mysql://u:p@h:3306/?timeout=5s", MySQL, "mysql://u:p@h:3306/?timeout=5s", ""},
		{"postgres://u:p@h/postgres?sslmode=require&search_path=x", Postgres, "postgres://u:p@h/postgres?sslmode=require&search_path=x", ""},
		{"u:p=x@tcp(h:3306)/", MySQL, "u:p=x@tcp(h:3306)/", ""},
		{"host=h user=u password=p", Postgres, "postgres://u:p@h:5432/", ""},
		{"host=h port=5433 dbname=d user=u password='p w\\'x' sslmode=verify-full", Postgres, "postgres://u:p%20w%27x@h:5433/?sslmode=verify-full", ""},
		{"password=s3cr;et host=db.prod dbname=x", Postgres, "postgres://:s3cr;et@db.prod:5432/", ""},
		{"dbname=x user=u password=p", Postgres, "", "the connection string names no server (Server=, Host=) (the value is not shown)"},
		{"Database=d;Uid=u;Pwd=p", MySQL, "", "the connection string names no server (Server=, Host=) (the value is not shown)"},
		{"Server=h;Uid=u;Pwd=Blue;Horse='x'y", MySQL, "", "connection string: text after the quoted value of pair 4"},
		{"jdbc:postgresql://h/db?user=u&password=p", Postgres, "postgres://u:p@h:5432/", ""},
		{"Server=h;Database=d;Uid=u;Pwd=p", MySQL, "mysql://u:p@h:3306/", ""},
		{"Host=h;Database=d;Username=u;Password=p", Postgres, "postgres://u:p@h:5432/", ""},
		{"Server=h;Uid=u;Pwd='unterminated", MySQL, "", "connection string: the value of pair 3 has no closing quote"},
		{"Server=(localdb)\\x;Trusted_Connection=yes", MySQL, "", "the DSN names SQL Server; DriftGate restores MySQL, MariaDB and PostgreSQL"},
	} {
		got, err := Convert(tt.in, tt.driver)
		if tt.err != "" {
			if err == nil || err.Error() != tt.err {
				t.Errorf("%s: error %v, want %s", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.out {
			t.Errorf("%s: %s, %v; want %s", tt.in, got, err, tt.out)
		}
	}
}

func TestCheckRef(t *testing.T) {
	for _, tt := range []struct {
		r   Ref
		err string
	}{
		{Ref{Source: Dotenv, File: ".env"}, ""},
		{Ref{Source: Dotenv, File: ".env", Key: "DATABASE_URL", Host: "127.0.0.1:3307"}, ""},
		{Ref{Source: Env, Key: "X"}, ""},
		{Ref{Source: Env, Key: "X", Host: "h"}, "env takes the name of a variable only"},
		{Ref{Source: AppSettings, File: "a.csproj"}, "appsettings needs key (the connection string, e.g. ConnectionStrings:Default)"},
		{Ref{Source: Compose, File: "compose.yaml"}, "compose needs key (the name of the database service)"},
		{Ref{Source: "toml", File: "x"}, `unknown source "toml"`},
		{Ref{Source: Dotenv, File: "../.env"}, "../.env must be inside the project"},
		{Ref{Source: Dotenv, File: ".env", Host: "u:p@h"}, "host must be host, host:port or the path of a socket"},
	} {
		err := CheckRef(tt.r)
		if (err == nil && tt.err != "") || (err != nil && err.Error() != tt.err) {
			t.Errorf("%+v: %v, want %q", tt.r, err, tt.err)
		}
	}
}
