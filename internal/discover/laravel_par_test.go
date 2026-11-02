package discover

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestLaravelRulesAtTheSameTime: route:list and the rules script run at the
// same time (route:list here waits until the script has started), and the
// log holds what route:list printed, then what the script printed. When
// route:list fails, the script's result and log are dropped, as when it ran
// only after a successful route:list.
func TestLaravelRulesAtTheSameTime(t *testing.T) {
	dir := t.TempDir()
	list := []string{"sh", "-c", `echo list-log >&2
for i in $(seq 1 100); do [ -f started ] && break; sleep 0.05; done
[ -f started ] || { echo "the rules script did not start" >&2; exit 1; }
sleep 0.2; echo list-done >&2
echo '[{"method":"POST","uri":"api/faq","name":null,"action":"Closure","middleware":["api"]}]'`}
	php := []string{"sh", "-c", `cat >/dev/null; touch started; echo rules-log >&2
echo '__DRIFTGATE_RULES__[{"methods":["POST"],"uri":"api/faq","domain":null,"file":"app/Faq.php","rules":{"q":["required","string"]}}]'`}
	var log bytes.Buffer
	rs, err := laravel(context.Background(), dir, Adapter{Command: list, PHP: php}, &log)
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if r := find(t, rs, "POST /api/faq"); r.BodySource != "validation rules" || r.File != "app/Faq.php" {
		t.Fatalf("route %+v", r)
	}
	if got := log.String(); got != "list-log\nlist-done\nrules-log\n" {
		t.Fatalf("log %q", got)
	}

	failing := []string{"sh", "-c", `echo list-log >&2; exit 3`}
	log.Reset()
	if _, err := laravel(context.Background(), dir, Adapter{Command: failing, PHP: php}, &log); err == nil || strings.Contains(err.Error(), "validation rules") {
		t.Fatalf("error %v", err)
	}
	if got := log.String(); got != "list-log\n" {
		t.Fatalf("log after a failed route:list %q", got)
	}

	broken := []string{"sh", "-c", `cat >/dev/null; echo rules-log >&2; exit 2`}
	log.Reset()
	if _, err := laravel(context.Background(), dir, Adapter{Command: list, PHP: broken}, &log); err == nil || !strings.Contains(err.Error(), "reading validation rules") {
		t.Fatalf("error %v", err)
	}
}
