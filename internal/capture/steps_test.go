package capture

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestCommandStepDoesNotSeeTheKey: a command step runs without the project
// key in its environment, like every other command DriftGate starts.
func TestCommandStepDoesNotSeeTheKey(t *testing.T) {
	t.Setenv(secret.EnvKey, "c2VjcmV0LWtleS1mb3ItYS10ZXN0LW9ubHktMDAwMDA=")
	r := &runner{p: &config.Project{Root: t.TempDir()}}
	res := r.command(context.Background(), catalog.Request{ID: "env", Command: []string{"sh", "-c", "printf %s \"$" + secret.EnvKey + "\""}, Output: true, Storage: snapshot.Public})
	if res.Record.Exit == nil || *res.Record.Exit != 0 {
		t.Fatalf("command: %+v", res.Record)
	}
	if strings.Contains(string(res.Body), "c2VjcmV0") {
		t.Fatal("the command saw the project key")
	}
}

// TestUploadsReadWhenSent: a real file is checked up front but not held:
// its request reads it when it is sent, so the files of a capture are not
// all in memory for its whole length; a file that changed after the start
// fails its request instead of being sent.
func TestUploadsReadWhenSent(t *testing.T) {
	uploads := t.TempDir()
	content := bytes.Repeat([]byte("name,value\nx,1\n"), 1000)
	if err := os.WriteFile(filepath.Join(uploads, "a.csv"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.EnvKey, "c2VjcmV0LWtleS1mb3ItYS10ZXN0LW9ubHktMDAwMDA=")
	p := &config.Project{Root: t.TempDir(), Config: config.Config{Project: "up", Uploads: config.Uploads{Dir: uploads}}}
	key, err := p.LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	req := catalog.Request{ID: "up", Method: "POST", Path: "/up", Multipart: &catalog.Multipart{
		Files: []catalog.File{{Field: "file", Path: "a.csv", Ref: key.Ref(content)}}}}
	ups, err := prepareUploads(p, []catalog.Request{req})
	if err != nil {
		t.Fatal(err)
	}
	if part := ups["up"].parts[0]; part.Data != nil || part.Size != int64(len(content)) {
		t.Fatalf("the file is held up front: %d bytes, size %d", len(part.Data), part.Size)
	}
	sent, err := ups["up"].apply(req)
	if err != nil || !bytes.Contains(sent.Body, content) {
		t.Fatalf("encoded when sent: %v", err)
	}
	changed := append([]byte("NAME"), content[4:]...)
	if err := os.WriteFile(filepath.Join(uploads, "a.csv"), changed, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ups["up"].apply(req); err == nil || !strings.Contains(err.Error(), "changed during the capture") {
		t.Fatalf("a file changed after the start: %v", err)
	}
}

// TestStepOutputNotLogged: when a command step fails, the log shows what
// it wrote to standard error, not its output, which is its body: private,
// or public but not scanned for personal data yet.
func TestStepOutputNotLogged(t *testing.T) {
	for _, storage := range []string{snapshot.Private, snapshot.Public} {
		var log bytes.Buffer
		r := &runner{p: &config.Project{Root: t.TempDir()}, log: &log}
		res := r.command(context.Background(), catalog.Request{ID: "export", Storage: storage, Output: true,
			Command: []string{"sh", "-c", "printf 'marko%sexample.rs\\n' @; echo export failed >&2; exit 3"}})
		if res.Record.Exit == nil || *res.Record.Exit != 3 {
			t.Fatalf("%s: %+v", storage, res.Record)
		}
		if !strings.Contains(log.String(), "export failed") || strings.Contains(log.String(), "marko@") {
			t.Fatalf("%s: log %q", storage, log.String())
		}
	}
}

// TestCommandLine: driftgate seed shows the whole command it runs.
// TestCommandLine: driftgate seed shows the whole command it runs, its
// passwords and secrets as ***; a reference to a variable stays.
func TestCommandLine(t *testing.T) {
	for _, c := range []struct {
		cmd  []string
		want string
	}{
		{[]string{"sh", "-c", `gunzip -c dump.sql.gz | mysql "app"`, ""}, `sh -c "gunzip -c dump.sql.gz | mysql \"app\"" ""`},
		{[]string{"mysql", "-u", "root", "-p", "s3cr"}, `mysql -u root -p ***`},
		{[]string{"mysql", "-ps3cr", "app"}, `mysql -p*** app`},
		{[]string{"mysql", "--password=s3cr", "app"}, `mysql --password=*** app`},
		{[]string{"sh", "-c", "sqlcmd -S db -U sa -P s3cr -Q x"}, `sh -c "sqlcmd -S db -U sa -P *** -Q x"`},
		{[]string{"sh", "-c", `sqlcmd -U sa -P "$MSSQL_SA_PASSWORD"`}, `sh -c "sqlcmd -U sa -P \"$MSSQL_SA_PASSWORD\""`},
		{[]string{"psql", "postgres://app:s3cr@db/app"}, `psql postgres://app:***@db/app`},
		{[]string{"tool", "Server=db;User Id=sa;Password=s3cr;Encrypt=true"}, `tool "Server=db;User Id=sa;Password=***;Encrypt=true"`},
		{[]string{"env", "API_TOKEN=s3cr", "MYSQL_PWD=s3cr", "DB_PASSWORD=s3cr", "APP_ENV=local"}, `env API_TOKEN=*** MYSQL_PWD=*** DB_PASSWORD=*** APP_ENV=local`},
		{[]string{"php", "artisan", "migrate:fresh", "--seed", "--force"}, `php artisan migrate:fresh --seed --force`},
	} {
		got := commandLine(c.cmd)
		if got != c.want || strings.Contains(got, "s3cr") {
			t.Errorf("%q: %s, want %s", c.cmd, got, c.want)
		}
	}
}
