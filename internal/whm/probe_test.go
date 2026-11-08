package whm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Shape of `manage_hooks list` as printed on a live cPanel 11.136 server.
const hooksOut = "PkgAcct:\n\tCreate:\n\t\thook: PodmanHooks::_do_backup\n\t\tstage: pre\n\t\texectype: module\n\t\tweight: 300\n\t\t--\n" +
	"\t\tstage: postFinalize\n\t\thook: Cpanel::Meridian::Hooks::Backup::post_finalize\n" +
	"System:\n\tBackup:\n\t\tstage: pre\n\t\thook: /usr/local/servatum/whm-backup.sh\n\t\texectype: script\n\t\t--\n" +
	"\t\tstage: post\n\t\thook: /opt/other/backup-notify\n" +
	"Domain:\n\treplace_temporary_domain:\n\t\tstage: post\n\t\thook: X::y\n"

func TestParseHooks(t *testing.T) {
	h := ParseHooks([]byte(hooksOut))
	if len(h) != 4 {
		t.Fatalf("hooks=%+v", h)
	}
	if h[0].Category != "PkgAcct" || h[0].Event != "Create" || h[0].Stage != "pre" || h[0].Hook != "PodmanHooks::_do_backup" {
		t.Fatalf("h0=%+v", h[0])
	}
	if !h[2].Ours || h[3].Ours || h[3].Stage != "post" || h[3].Event != "Backup" {
		t.Fatalf("h2=%+v h3=%+v", h[2], h[3])
	}
}

func TestFindSets(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"2026-11-08", "weekly/2026-11-09", "monthly/2026-11-01", "custom/x/y/z"} {
		_ = os.MkdirAll(filepath.Join(root, d, "accounts"), 0o700)
	}
	tgz(t, filepath.Join(root, "2026-11-08", "accounts", "alice.tar.gz"), 20*1024)
	_ = os.MkdirAll(filepath.Join(root, "dominion_mailbox_stage", "acct"), 0o700) // not a set
	sets := FindSets(root)
	if len(sets) != 3 {
		t.Fatalf("sets=%+v", sets) // custom/x/y/z/accounts is too deep
	}
	if sets[0].Tier != "weekly" || sets[1].Tier != "daily" || sets[1].Archives != 1 || sets[2].Tier != "monthly" {
		t.Fatalf("sets=%+v", sets)
	}
}

func TestProbeNoLocalSets(t *testing.T) {
	root := t.TempDir() // backup dir: no sets on disk
	logs := t.TempDir()
	cron := t.TempDir()
	oldLog, oldCron := LogDir, CronDir
	LogDir, CronDir = logs, cron
	t.Cleanup(func() { LogDir, CronDir = oldLog, oldCron })
	_ = os.WriteFile(filepath.Join(logs, "1.log"), []byte("Final state is Backup::Success (0)\n"), 0o600)
	script := filepath.Join(t.TempDir(), "offsite_sync.sh")
	_ = os.WriteFile(script, []byte("#!/bin/bash\nBACKUP_DIR=\""+root+"\"\nrclone sync ...\n"), 0o700)
	_ = os.WriteFile(filepath.Join(cron, "helper"), []byte("MAILTO=root\n30 2 * * * root rm -rf "+root+"/old\n"), 0o600)
	crontab := "0 1 * * * " + script + " >/dev/null 2>&1\n0 0 * * * /usr/local/cpanel/bin/backup\n15 * * * * /bin/true\n" +
		"*/15 * * * * /usr/bin/python3 /opt/scripts" + root + "/status.py build\n" + // path merely contains the dir name
		"5 * * * * /usr/bin/python3 -u " + script + "\n" // interpreter + script

	base := FakeExec(root)
	x := func(name string, args ...string) ([]byte, error) {
		j := strings.Join(args, " ")
		switch {
		case name == "crontab":
			return []byte(crontab), nil
		case strings.HasSuffix(name, "manage_hooks"):
			return []byte(hooksOut), nil
		case strings.Contains(j, "backup_destination_list"):
			return []byte(`{"data":{"destination_list":[]},"metadata":{"result":1,"reason":"OK"}}`), nil
		}
		return base(name, args...)
	}
	p := RunProbe(x, time.Now())
	if len(p.Errors) != 0 {
		t.Fatalf("errors=%v", p.Errors)
	}
	if !p.Config.Enabled || !p.Config.KeepLocal || p.Config.RetainDaily != 1 || p.Accounts.Included != 3 || p.Accounts.Total != 5 {
		t.Fatalf("probe=%+v", p)
	}
	if strings.Join(p.Accounts.BackupOff, ",") != "dave" || p.Accounts.SuspendedExcluded != 1 {
		t.Fatalf("accounts=%+v", p.Accounts)
	}
	if len(p.Cron) != 3 || p.Cron[0].Command != script || p.Cron[0].Why != "script mentions the backup dir" || p.Cron[0].Schedule != "0 1 * * *" {
		t.Fatalf("cron=%+v", p.Cron)
	}
	if p.Cron[1].Command != script || p.Cron[1].Schedule != "5 * * * *" {
		t.Fatalf("interpreter line=%+v", p.Cron[1])
	}
	if p.Cron[2].Schedule != "30 2 * * *" || p.Cron[2].Command != "rm" || p.Cron[2].Why != "line mentions the backup dir" {
		t.Fatalf("cron.d=%+v", p.Cron[2])
	}
	joined := strings.Join(p.Hints, " | ")
	if !strings.Contains(joined, "something removes them") || !strings.Contains(joined, script) || !strings.Contains(joined, "/opt/other/backup-notify") {
		t.Fatalf("hints=%v", p.Hints)
	}
	if p.Disk == nil || p.Disk.TotalBytes == 0 {
		t.Fatal("no disk info")
	}
}

func TestMentionsDir(t *testing.T) {
	for s, want := range map[string]bool{
		"rsync /backup/ x":                   true,
		`BACKUP_DIR="/backup"`:               true,
		"BACKUP_DIR=/backup":                 true,
		"ls /backup":                         true,
		"/opt/scripts/backup/offsite_sync.sh": false,
		"/usr/bin/python3 /root/backup/b.py": false,
		"find /backupx":                      false,
	} {
		if got := mentionsDir(s, "/backup"); got != want {
			t.Errorf("%q: got %v", s, got)
		}
	}
}
