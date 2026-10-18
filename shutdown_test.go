package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestHelperRunMain is not a test on its own: TestSIGTERMExitsZero re-runs the
// test binary with SHELF_RUN_MAIN=1 so this runs the real main() in a child.
func TestHelperRunMain(t *testing.T) {
	if os.Getenv("SHELF_RUN_MAIN") != "1" {
		t.Skip("helper for TestSIGTERMExitsZero")
	}
	main()
}

// TestSIGTERMExitsZero is what `docker stop` (and so idlegate) does: SIGTERM,
// then SIGKILL after the grace period. The process must exit 0, well inside it.
// The child isn't PID 1, so this can't catch a broken Dockerfile.
func TestSIGTERMExitsZero(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the server in a subprocess")
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "library")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperRunMain$")
	cmd.Env = append(os.Environ(), "SHELF_RUN_MAIN=1", "ROOT_DIR="+lib,
		"CACHE_DIR="+filepath.Join(dir, "cache"), "LISTEN_ADDR="+addr,
		"API_KEY=a-long-enough-test-key", "OLLAMA_URL=")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Ready means /healthz answers 200 without a key.
	url := fmt.Sprintf("http://%s/healthz", addr)
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("/healthz = %d, want 200", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("not ready within 10s: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	start := time.Now()
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v, want exit code 0", err)
		}
		if el := time.Since(start); el > 5*time.Second {
			t.Fatalf("took %s to stop, want under 5s", el)
		}
	case <-time.After(6 * time.Second):
		cmd.Process.Kill()
		t.Fatal("still running 6s after SIGTERM")
	}
}

// A scan cut off by shutdown must not prune: the walk saw nothing, but the
// archives are still there.
func TestScanCancelledDoesNotPrune(t *testing.T) {
	dir := t.TempDir()
	idx, err := openIndexDB(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &App{cfg: Config{RootDir: dir, CoversDir: dir}, index: idx, ctx: ctx}
	a.writeResult(result{rel: "1 A.cbz", title: "A"})

	a.Scan()

	var n int
	idx.QueryRow(`SELECT COUNT(*) FROM archives`).Scan(&n)
	if n != 1 || a.scanning.Load() {
		t.Fatalf("rows=%d scanning=%v after cancelled scan, want 1/false", n, a.scanning.Load())
	}
}

func TestWriteFileAtomicLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "c.jpg")
	if err := writeFileAtomic(dst, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(dst, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "two" {
		t.Fatalf("content = %q, want two", b)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(m) != 0 {
		t.Fatalf("temp files left behind: %v", m)
	}
	// A leftover from a killed write is swept on start.
	stale := filepath.Join(dir, ".c.jpg.123.tmp")
	os.WriteFile(stale, []byte("partial"), 0o644)
	sweepTempCovers(dir)
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale temp not swept: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("real cover removed by sweep: %v", err)
	}
}

func TestUnavailable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New("dial tcp: connection refused"), true},
		{context.DeadlineExceeded, true},
		{&statusError{code: 503}, true},
		{&statusError{code: 500}, false},
		{&statusError{code: 400}, false},
	} {
		if got := unavailable(tc.err); got != tc.want {
			t.Errorf("unavailable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
