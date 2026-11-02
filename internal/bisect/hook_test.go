package bisect

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	ctx := context.Background()
	dir := t.TempDir()
	var log bytes.Buffer

	// The command runs from dir and gets the variables.
	err := Hook(ctx, dir, []string{"sh", "-c", `printf '%s' "$` + EnvCommit + `" > got`}, []string{EnvCommit + "=abc"}, time.Minute, &log)
	if raw, _ := os.ReadFile(filepath.Join(dir, "got")); err != nil || string(raw) != "abc" {
		t.Fatalf("%v %q", err, raw)
	}

	// An application started in the background does not hold the hook.
	began := time.Now()
	if err := Hook(ctx, dir, []string{"sh", "-c", "echo starting; sleep 5 &"}, nil, time.Minute, &log); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("hook waited for the background process: %s", d)
	}

	// A failure shows the last lines of the output.
	log.Reset()
	if err := Hook(ctx, dir, []string{"sh", "-c", "echo build failed; exit 2"}, nil, time.Minute, &log); err == nil || !strings.Contains(log.String(), "build failed") {
		t.Errorf("%v %q", err, log.String())
	}

	// The time limit stops it.
	began = time.Now()
	if err := Hook(ctx, dir, []string{"sleep", "10"}, nil, 200*time.Millisecond, nil); err == nil || !strings.Contains(err.Error(), "within") {
		t.Errorf("timeout: %v", err)
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("timeout took %s", d)
	}
}

func TestWait(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	ctx := context.Background()
	dir := t.TempDir()
	flag := filepath.Join(dir, "up")
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.WriteFile(flag, nil, 0o644)
	}()
	if err := Wait(ctx, dir, []string{"test", "-e", flag}, nil, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := Wait(ctx, dir, []string{"false"}, nil, 300*time.Millisecond); err == nil {
		t.Error("ready that never succeeds")
	}
}
