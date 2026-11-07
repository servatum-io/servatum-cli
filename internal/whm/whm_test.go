package whm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Shapes follow the response samples in cPanel's WHM API 1 docs; numbers
// as strings ("1") are what a live server returned for some fields.
const cfgJSON = `{"data":{"backup_config":{"backupenable":1,"backup_daily_enable":1,"backup_daily_retention":"1",
"backupdays":"0,2,3,4,5,6","backupdir":"%s","backuptype":"compressed","keeplocal":"1","backupsuspendedaccounts":0}},
"metadata":{"command":"backup_config_get","reason":"OK","result":1,"version":1}}`

const acctJSON = `{"data":{"acct":[
{"user":"alice","backup":1,"suspended":0},{"user":"bob","backup":1,"suspended":0},
{"user":"carol","backup":1,"suspended":0},{"user":"dave","backup":0,"suspended":0},
{"user":"erin","backup":1,"suspended":1}]},"metadata":{"command":"listaccts","reason":"OK","result":1,"version":1}}`

func FakeExec(dir string) Exec {
	return func(name string, args ...string) ([]byte, error) {
		switch {
		case strings.Contains(strings.Join(args, " "), "backup_config_get"):
			return []byte(strings.Replace(cfgJSON, "%s", dir, 1)), nil
		case strings.Contains(strings.Join(args, " "), "listaccts"):
			return []byte(acctJSON), nil
		}
		return nil, os.ErrNotExist
	}
}

func tgz(t *testing.T, path string, payload int) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := bytes.Repeat([]byte{'x'}, payload)
	// random-ish content so gzip can't shrink it below the size floor
	rand.New(rand.NewSource(1)).Read(body)
	_ = tw.WriteHeader(&tar.Header{Name: "u/homedir/f", Mode: 0o600, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigAndExpected(t *testing.T) {
	x := FakeExec("/backup")
	cfg, err := GetConfig(x)
	if err != nil || cfg.BackupDir != "/backup" || cfg.BackupType != "compressed" || cfg.BackupEnable != 1 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	accts, err := ListAccounts(x)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(Expected(cfg, accts), ",")
	if got != "alice,bob,carol" { // dave: backup off; erin: suspended, not backed up
		t.Fatalf("expected=%s", got)
	}
}

func TestScanEvaluateVerify(t *testing.T) {
	root := t.TempDir()
	day := time.Date(2026, 11, 8, 0, 0, 0, 0, time.Local)
	acc := filepath.Join(root, "2026-11-08", "accounts")
	_ = os.MkdirAll(acc, 0o700)
	tgz(t, filepath.Join(acc, "alice.tar.gz"), 64*1024)
	tgz(t, filepath.Join(acc, "bob.tar.gz"), 64*1024)
	// carol missing; a corrupt one for a user the server no longer lists
	_ = os.WriteFile(filepath.Join(acc, "zed.tar.gz"), bytes.Repeat([]byte{1}, 20*1024), 0o600)

	cfg, _ := GetConfig(FakeExec(root))
	accts, _ := ListAccounts(FakeExec(root))
	dirs := RunDirs(root, day)
	if len(dirs) != 1 {
		t.Fatalf("dirs=%v", dirs)
	}
	arch, err := Scan(dirs)
	if err != nil || len(arch) != 3 {
		t.Fatalf("arch=%v err=%v", arch, err)
	}
	v := Verify(context.Background(), arch, 3, 100, rand.New(rand.NewSource(1)))
	if v.Sampled != 3 || v.OK != 2 || len(v.Failed) != 1 || v.Failed[0] != "zed" {
		t.Fatalf("verify=%+v", v)
	}
	r := Evaluate(cfg, Expected(cfg, accts), dirs, arch, &v)
	if r.Status != "fail" || !strings.Contains(r.Message, "1 account(s) missing") || !strings.Contains(r.Message, "do not open") {
		t.Fatalf("result=%+v", r)
	}
	if r.Details.AccountsExpected != 3 || r.Details.Archives != 3 || r.Details.TotalBytes == 0 {
		t.Fatalf("details=%+v", r.Details)
	}
}

func TestEvaluateOKAndNoDir(t *testing.T) {
	root := t.TempDir()
	day := time.Date(2026, 11, 9, 0, 0, 0, 0, time.Local) // Monday: weekly set
	acc := filepath.Join(root, "weekly", "2026-11-09", "accounts")
	_ = os.MkdirAll(acc, 0o700)
	for _, u := range []string{"alice", "bob", "carol"} {
		tgz(t, filepath.Join(acc, u+".tar.gz"), 32*1024)
	}
	cfg, _ := GetConfig(FakeExec(root))
	accts, _ := ListAccounts(FakeExec(root))
	exp := Expected(cfg, accts)
	dirs := RunDirs(root, day)
	arch, _ := Scan(dirs)
	v := Verify(context.Background(), arch, 3, 5, rand.New(rand.NewSource(2)))
	if v.Sampled != 1 { // 5% of 3 rounds to 0, floor is 1
		t.Fatalf("verify=%+v", v)
	}
	if r := Evaluate(cfg, exp, dirs, arch, &v); r.Status != "ok" {
		t.Fatalf("result=%+v", r)
	}
	none := RunDirs(root, day.AddDate(0, 0, 1))
	if r := Evaluate(cfg, exp, none, map[string]Archive{}, nil); r.Status != "fail" || !strings.Contains(r.Message, "no backup directory") {
		t.Fatalf("result=%+v", r)
	}
}

func TestTooSmallAndTimeout(t *testing.T) {
	root := t.TempDir()
	day := time.Date(2026, 11, 8, 0, 0, 0, 0, time.Local)
	acc := filepath.Join(root, "2026-11-08", "accounts")
	_ = os.MkdirAll(acc, 0o700)
	tgz(t, filepath.Join(acc, "alice.tar.gz"), 64*1024)
	tgz(t, filepath.Join(acc, "bob.tar.gz"), 64*1024)
	tgz(t, filepath.Join(acc, "carol.tar.gz"), 10) // tiny
	cfg, _ := GetConfig(FakeExec(root))
	accts, _ := ListAccounts(FakeExec(root))
	dirs := RunDirs(root, day)
	arch, _ := Scan(dirs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := Verify(ctx, arch, 3, 100, rand.New(rand.NewSource(1)))
	if !v.Incomplete || v.Sampled != 0 {
		t.Fatalf("verify=%+v", v)
	}
	r := Evaluate(cfg, Expected(cfg, accts), dirs, arch, &v)
	if r.Status != "warn" || !strings.Contains(r.Message, "under 10 KB") || !strings.Contains(r.Message, "ran out of time") {
		t.Fatalf("result=%+v", r)
	}
}

func TestProblemsCapped(t *testing.T) {
	exp := make([]string, 150)
	for i := range exp {
		exp[i] = "u" + strings.Repeat("x", i%5) + string(rune('a'+i%26))
	}
	r := Evaluate(Config{}, exp, []string{"/x"}, map[string]Archive{}, nil)
	if len(r.Details.Problems) != MaxProblems || r.Details.ProblemsTruncated != 50 {
		t.Fatalf("problems=%d truncated=%d", len(r.Details.Problems), r.Details.ProblemsTruncated)
	}
	if n := len(r.Details.JSON()); n > 12*1024 {
		t.Fatalf("details %d bytes, too close to the 16 KB body limit", n)
	}
}
