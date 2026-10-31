package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/servatum-io/servatum-cli/internal/api"
	"github.com/servatum-io/servatum-cli/internal/mockapi"
)

func setup(t *testing.T) (*mockapi.Server, string) {
	m := mockapi.New()
	m.Keys["sv_ing_test"] = true
	m.Tokens["pt_test"] = true
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	t.Setenv("SERVATUM_KEY", "sv_ing_test")
	t.Setenv("SERVATUM_ENDPOINT", srv.URL)
	t.Setenv("SERVATUM_JOB", "")
	t.Setenv("SERVATUM_SPOOL", t.TempDir())
	api.RetryDelays = []time.Duration{0, 0}
	return m, srv.URL
}

func spooled(t *testing.T) int {
	es, _ := filepath.Glob(filepath.Join(os.Getenv("SERVATUM_SPOOL"), "*.json"))
	return len(es)
}

func run(args ...string) (int, string, string) {
	var o, e bytes.Buffer
	c := dispatch(args, &o, &e)
	return c, o.String(), e.String()
}

func TestRunOK(t *testing.T) {
	m, _ := setup(t)
	code, out, _ := run("run", "--job", "job_1", "--send-output", "--", "sh", "-c", "echo hello; echo err >&2")
	if code != 0 || !strings.Contains(out, "hello") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	runs, _ := m.Snapshot()
	if len(runs) != 2 || runs[0]["status"] != "started" || runs[1]["status"] != "ok" {
		t.Fatalf("runs=%v", runs)
	}
	if msg := runs[1]["message"].(string); !strings.Contains(msg, "hello") || !strings.Contains(msg, "err") {
		t.Fatalf("message=%q", msg)
	}
	if runs[1]["exit_code"].(float64) != 0 || runs[1]["finished_at"] == nil {
		t.Fatalf("run=%v", runs[1])
	}
}

func TestRunFailKeepsExitCode(t *testing.T) {
	m, _ := setup(t)
	code, _, _ := run("run", "--job", "job_1", "--no-start", "--", "sh", "-c", "exit 7")
	runs, _ := m.Snapshot()
	if code != 7 || len(runs) != 1 || runs[0]["status"] != "fail" || runs[0]["exit_code"].(float64) != 7 {
		t.Fatalf("code=%d runs=%v", code, runs)
	}
}

func TestRunWarnCodes(t *testing.T) {
	m, _ := setup(t)
	code, _, _ := run("run", "--job", "job_1", "--no-start", "--warn-exit-codes", "24", "--", "sh", "-c", "exit 24")
	runs, _ := m.Snapshot()
	if code != 24 || runs[0]["status"] != "warn" {
		t.Fatalf("code=%d runs=%v", code, runs)
	}
}

func TestRunJobFromEnv(t *testing.T) {
	m, _ := setup(t)
	t.Setenv("SERVATUM_JOB", "job_env")
	run("run", "--no-start", "--", "true")
	runs, _ := m.Snapshot()
	if len(runs) != 1 || runs[0]["job"] != "job_env" {
		t.Fatalf("runs=%v", runs)
	}
}

// The backup must behave exactly as without servatum when reporting breaks.
func TestRunAPIDownStillReturnsCommandCode(t *testing.T) {
	setup(t)
	t.Setenv("SERVATUM_ENDPOINT", "http://127.0.0.1:1")
	code, out, errS := run("run", "--job", "job_1", "--", "sh", "-c", "echo data; exit 0")
	if code != 0 || !strings.Contains(out, "data") || !strings.Contains(errS, "spooled") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errS)
	}
}

func TestRunBadKeyStillRuns(t *testing.T) {
	m, _ := setup(t)
	t.Setenv("SERVATUM_KEY", "sv_ing_wrong")
	code, _, errS := run("run", "--job", "job_1", "--", "sh", "-c", "exit 3")
	runs, _ := m.Snapshot()
	if code != 3 || len(runs) != 0 || !strings.Contains(errS, "401") {
		t.Fatalf("code=%d runs=%v err=%q", code, runs, errS)
	}
}

func TestRunNoKeyStillRuns(t *testing.T) {
	setup(t)
	os.Unsetenv("SERVATUM_KEY")
	code, out, errS := run("run", "--job", "job_1", "--", "sh", "-c", "echo x")
	if code != 0 || out != "x\n" || !strings.Contains(errS, "no ingest key") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errS)
	}
}

func TestRunMissingCommand(t *testing.T) {
	setup(t)
	if code, _, _ := run("run", "--job", "job_1", "--", "/nonexistent/binary"); code != 127 {
		t.Fatalf("code=%d", code)
	}
}

func TestRunLargeOutputFitsBody(t *testing.T) {
	m, _ := setup(t)
	// 200 KB of output with quotes/backslashes that inflate under JSON escaping.
	code, _, errS := run("run", "--job", "job_1", "--no-start", "--quiet", "--send-output", "--",
		"sh", "-c", `i=0; while [ $i -lt 4000 ]; do echo "\"line\" \\ $i ünïcødé"; i=$((i+1)); done`)
	runs, _ := m.Snapshot()
	if code != 0 || len(runs) != 1 {
		t.Fatalf("code=%d runs=%d err=%q", code, len(runs), errS)
	}
	if !strings.Contains(runs[0]["message"].(string), "line\" \\ 3999") {
		t.Fatal("tail of output not kept")
	}
}

func TestIdempotencyDedup(t *testing.T) {
	m, _ := setup(t)
	run("run", "--job", "job_1", "--no-start", "--idempotency-key", "k1", "--", "true")
	run("run", "--job", "job_1", "--no-start", "--idempotency-key", "k1", "--", "true")
	if runs, _ := m.Snapshot(); len(runs) != 1 {
		t.Fatalf("runs=%d", len(runs))
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	m, _ := setup(t)
	_, _, errS := run("run", "--job", "job_1", "--dry-run", "--", "true")
	runs, _ := m.Snapshot()
	if len(runs) != 0 || !strings.Contains(errS, `"status": "ok"`) {
		t.Fatalf("runs=%v err=%q", runs, errS)
	}
}

func TestPing(t *testing.T) {
	m, _ := setup(t)
	if c, _, e := run("ping", "pt_test"); c != 0 {
		t.Fatal(e)
	}
	if c, _, e := run("ping", "--start", "pt_test"); c != 0 {
		t.Fatal(e)
	}
	f := t.TempDir() + "/log"
	os.WriteFile(f, []byte("disk full"), 0o600)
	if c, _, e := run("ping", "--fail", "--log", f, "pt_test"); c != 0 {
		t.Fatal(e)
	}
	_, p := m.Snapshot()
	if len(p) != 3 || p[0].Status != "" || p[1].Status != "start" || p[2].Status != "fail" || p[2].Body != "disk full" || p[2].Method != "POST" {
		t.Fatalf("pings=%+v", p)
	}
	if c, _, _ := run("ping", "pt_bad"); c != 3 {
		t.Fatalf("bad token code=%d, want 3", c)
	}
}

func TestRunOutputNotSentByDefault(t *testing.T) {
	m, _ := setup(t)
	code, out, _ := run("run", "--job", "job_1", "--no-start", "--", "sh", "-c", "echo /home/secret/path")
	runs, _ := m.Snapshot()
	if code != 0 || !strings.Contains(out, "/home/secret/path") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if msg, ok := runs[0]["message"]; ok && msg != "" {
		t.Fatalf("output leaked into report: %v", msg)
	}
}

func TestRetryThenDeliver(t *testing.T) {
	m, _ := setup(t)
	m.FailNext = 2 // two 503s, third attempt succeeds
	code, _, _ := run("run", "--job", "job_1", "--no-start", "--", "true")
	runs, _ := m.Snapshot()
	if code != 0 || len(runs) != 1 || spooled(t) != 0 {
		t.Fatalf("code=%d runs=%d spooled=%d", code, len(runs), spooled(t))
	}
}

func TestSpoolWhenDownThenFlushOnNextRun(t *testing.T) {
	m, _ := setup(t)
	m.FailNext = 3 // all inline attempts fail
	code, _, errS := run("run", "--job", "job_1", "--no-start", "--", "sh", "-c", "exit 4")
	if code != 4 || spooled(t) != 1 || !strings.Contains(errS, "spooled") {
		t.Fatalf("code=%d spooled=%d err=%q", code, spooled(t), errS)
	}
	// next run: spooled report goes first, then the new one
	run("run", "--job", "job_1", "--no-start", "--", "true")
	runs, _ := m.Snapshot()
	if spooled(t) != 0 || len(runs) != 2 || runs[0]["status"] != "fail" || runs[1]["status"] != "ok" {
		t.Fatalf("spooled=%d runs=%v", spooled(t), runs)
	}
}

func TestFlushCommandAndIdempotency(t *testing.T) {
	m, _ := setup(t)
	m.FailNext = 3
	run("run", "--job", "job_1", "--no-start", "--idempotency-key", "k9", "--", "true")
	if c, _, e := run("flush"); c != 0 {
		t.Fatalf("flush code=%d err=%q", c, e)
	}
	if c, _, _ := run("flush"); c != 0 { // nothing left, still fine
		t.Fatal("second flush failed")
	}
	runs, _ := m.Snapshot()
	if len(runs) != 1 || spooled(t) != 0 {
		t.Fatalf("runs=%d spooled=%d", len(runs), spooled(t))
	}
}

func TestBadKeyNotSpooled(t *testing.T) {
	setup(t)
	t.Setenv("SERVATUM_KEY", "sv_ing_wrong")
	run("run", "--job", "job_1", "--no-start", "--", "true")
	if spooled(t) != 0 {
		t.Fatalf("401 must not be spooled, got %d", spooled(t))
	}
}

func TestOtherKeysReportsLeftAlone(t *testing.T) {
	m, _ := setup(t)
	m.Keys["sv_ing_other"] = true
	m.FailNext = 3
	run("run", "--job", "job_1", "--no-start", "--", "true") // spooled under sv_ing_test
	t.Setenv("SERVATUM_KEY", "sv_ing_other")
	run("flush")
	if spooled(t) != 1 {
		t.Fatalf("report of another key was touched: spooled=%d", spooled(t))
	}
}

func TestPingNeverSpooled(t *testing.T) {
	m, _ := setup(t)
	m.FailNext = 3
	if c, _, _ := run("ping", "pt_test"); c != 1 {
		t.Fatalf("ping code=%d", c)
	}
	if spooled(t) != 0 {
		t.Fatal("ping was spooled")
	}
}

func TestStartedNotSpooled(t *testing.T) {
	setup(t)
	t.Setenv("SERVATUM_ENDPOINT", "http://127.0.0.1:1")
	run("run", "--job", "job_1", "--", "true")
	if spooled(t) != 1 { // only the final report
		t.Fatalf("spooled=%d, want 1", spooled(t))
	}
}
