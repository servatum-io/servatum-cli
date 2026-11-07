package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeWHM(t *testing.T, backupDir string) *[]string {
	var calls []string
	cfg := `{"data":{"backup_config":{"backupenable":1,"backupdir":"` + backupDir + `","backuptype":"compressed"}},"metadata":{"result":1,"reason":"OK"}}`
	acct := `{"data":{"acct":[{"user":"alice","backup":1,"suspended":0},{"user":"bob","backup":1,"suspended":0}]},"metadata":{"result":1,"reason":"OK"}}`
	old := whmExec
	whmExec = func(name string, args ...string) ([]byte, error) {
		j := strings.Join(args, " ")
		calls = append(calls, filepath.Base(name)+" "+j)
		switch {
		case strings.Contains(j, "backup_config_get"):
			return []byte(cfg), nil
		case strings.Contains(j, "listaccts"):
			return []byte(acct), nil
		}
		return []byte("ok"), nil
	}
	t.Cleanup(func() { whmExec = old })
	return &calls
}

func writeTgz(t *testing.T, path string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := make([]byte, 40*1024)
	rand.New(rand.NewSource(3)).Read(body)
	_ = tw.WriteHeader(&tar.Header{Name: "f", Mode: 0o600, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWHMHookPrePost(t *testing.T) {
	m, _ := setup(t)
	t.Setenv("SERVATUM_STATE", t.TempDir())
	root := t.TempDir()
	fakeWHM(t, root)
	start := time.Date(2026, 11, 8, 0, 0, 1, 0, time.Local)
	whmNow = func() time.Time { return start }
	t.Cleanup(func() { whmNow = time.Now })
	acc := filepath.Join(root, "2026-11-08", "accounts")
	_ = os.MkdirAll(acc, 0o700)
	writeTgz(t, filepath.Join(acc, "alice.tar.gz"))
	writeTgz(t, filepath.Join(acc, "bob.tar.gz"))

	whmStdin = strings.NewReader(`{"context":{"category":"System","event":"Backup","stage":"pre"},"data":{},"hook":{"stage":"pre"}}`)
	if c, _, e := run("whm", "hook", "--job", "job_w"); c != 0 {
		t.Fatalf("pre code=%d %s", c, e)
	}
	whmNow = func() time.Time { return start.Add(42 * time.Minute) }
	whmStdin = strings.NewReader(`{"context":{"category":"System","event":"Backup","stage":"post"},"data":{},"hook":{"stage":"post"}}`)
	if c, _, e := run("whm", "hook", "--job", "job_w"); c != 0 {
		t.Fatalf("post code=%d %s", c, e)
	}
	whmStdin = os.Stdin
	runs, _ := m.Snapshot()
	if len(runs) != 2 || runs[0]["status"] != "started" || runs[1]["status"] != "ok" {
		t.Fatalf("runs=%v", runs)
	}
	r := runs[1]
	if r["kind"] != "whm.backup.v1" || r["started_at"] == nil || r["size_bytes"].(float64) == 0 {
		t.Fatalf("run=%v", r)
	}
	d := r["details"].(map[string]any)
	if d["accounts_expected"].(float64) != 2 || d["archives"].(float64) != 2 {
		t.Fatalf("details=%v", d)
	}
	if v := d["verify"].(map[string]any); v["ok"].(float64) != 1 { // 5% of 2 -> floor 1
		t.Fatalf("verify=%v", v)
	}
}

func TestWHMReportMissedRun(t *testing.T) {
	m, _ := setup(t)
	t.Setenv("SERVATUM_STATE", t.TempDir())
	fakeWHM(t, t.TempDir()) // empty backup dir: nothing ran
	if c, _, e := run("whm", "report", "--job", "job_w", "--day", "2026-11-08"); c != 0 {
		t.Fatalf("code=%d %s", c, e)
	}
	runs, _ := m.Snapshot()
	if len(runs) != 1 || runs[0]["status"] != "fail" || !strings.Contains(runs[0]["message"].(string), "no backup directory") {
		t.Fatalf("runs=%v", runs)
	}
}

func TestWHMDryRunSendsNothing(t *testing.T) {
	m, _ := setup(t)
	t.Setenv("SERVATUM_STATE", t.TempDir())
	t.Setenv("SERVATUM_DRY_RUN", "1")
	fakeWHM(t, t.TempDir())
	c, _, e := run("whm", "report", "--day", "2026-11-08")
	if c != 0 || !strings.Contains(e, `"kind": "whm.backup.v1"`) {
		t.Fatalf("code=%d stderr=%s", c, e)
	}
	if runs, _ := m.Snapshot(); len(runs) != 0 {
		t.Fatalf("dry run sent %v", runs)
	}
}

func TestInstallUninstall(t *testing.T) {
	calls := fakeWHM(t, "/backup")
	dir := t.TempDir()
	oldP, oldE := whmPrefix, whmEnvFile
	whmPrefix, whmEnvFile = filepath.Join(dir, "servatum"), filepath.Join(dir, "etc", "env")
	t.Cleanup(func() { whmPrefix, whmEnvFile = oldP, oldE })
	_ = os.MkdirAll(filepath.Dir(whmEnvFile), 0o700)
	_ = os.WriteFile(whmEnvFile, []byte("SERVATUM_KEY=sv_ing_keep\nOTHER=1\n"), 0o644)
	t.Setenv("SERVATUM_KEY", "")

	if c, _, e := run("install", "whm", "--dry-run"); c != 0 {
		t.Fatalf("install code=%d %s", c, e)
	}
	env, _ := os.ReadFile(whmEnvFile)
	if s := string(env); !strings.Contains(s, "SERVATUM_KEY=sv_ing_keep") || !strings.Contains(s, "OTHER=1") || !strings.Contains(s, "SERVATUM_DRY_RUN=1") {
		t.Fatalf("env=%q", s)
	}
	if fi, _ := os.Stat(whmEnvFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("env mode %v", fi.Mode().Perm())
	}
	script, _ := os.ReadFile(filepath.Join(whmPrefix, "whm-backup.sh"))
	if !strings.Contains(string(script), "whm hook") || !strings.Contains(string(script), "exit 0") {
		t.Fatalf("script=%s", script)
	}
	want := "manage_hooks add script " + filepath.Join(whmPrefix, "whm-backup.sh") + " --manual --category System --event Backup --stage=post"
	if j, _ := json.Marshal(*calls); !strings.Contains(string(j), want) {
		t.Fatalf("calls=%v", *calls)
	}
	if c, _, e := run("uninstall", "whm"); c != 0 {
		t.Fatalf("uninstall code=%d %s", c, e)
	}
	if _, err := os.Stat(filepath.Join(whmPrefix, "whm-backup.sh")); !os.IsNotExist(err) {
		t.Fatal("hook script not removed")
	}
	if last := (*calls)[len(*calls)-1]; !strings.Contains(last, "delete script") || !strings.Contains(last, "--stage=post") {
		t.Fatalf("last call=%s", last)
	}
}
