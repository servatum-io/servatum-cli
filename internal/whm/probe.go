package whm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Probe is a read-only snapshot of how a server's cPanel backup is set up.
// It holds facts only; deciding what they mean is the engine's job. Hints
// are the few contradictions the CLI can already name on its own.
type Probe struct {
	Kind         string        `json:"kind"` // whm.probe.v1
	At           time.Time     `json:"at"`
	Config       ProbeConfig   `json:"config"`
	Destinations []Destination `json:"destinations"`
	Accounts     ProbeAccounts `json:"accounts"`
	LocalSets    []LocalSet    `json:"local_sets"`
	Logs         []LogState    `json:"logs"`
	Hooks        []Hook        `json:"hooks"`
	Cron         []CronRef     `json:"cron"`
	Disk         *Disk         `json:"disk,omitempty"`
	Errors       []string      `json:"errors,omitempty"` // sources that could not be read
	Hints        []string      `json:"hints,omitempty"`
}

type ProbeConfig struct {
	Enabled          bool   `json:"enabled"`
	Dir              string `json:"dir"`
	Type             string `json:"type"`
	Daily            bool   `json:"daily"`
	Days             string `json:"days"` // 0=Sun … 6=Sat
	Weekly           bool   `json:"weekly"`
	WeeklyDay        string `json:"weekly_day,omitempty"`
	Monthly          bool   `json:"monthly"`
	MonthlyDates     string `json:"monthly_dates,omitempty"`
	RetainDaily      int    `json:"retain_daily"`
	RetainWeekly     int    `json:"retain_weekly,omitempty"`
	RetainMonthly    int    `json:"retain_monthly,omitempty"`
	KeepLocal        bool   `json:"keep_local"`
	MetadataDisabled bool   `json:"metadata_disabled,omitempty"`
}

type Destination struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Disabled bool   `json:"disabled,omitempty"`
}

type ProbeAccounts struct {
	Total             int      `json:"total"`
	Included          int      `json:"included"`
	BackupOff         []string `json:"backup_off,omitempty"`
	SuspendedExcluded int      `json:"suspended_excluded,omitempty"`
}

type LocalSet struct {
	Path     string `json:"path"`
	Tier     string `json:"tier"` // daily | weekly | monthly | other
	Date     string `json:"date,omitempty"`
	Archives int    `json:"archives"`
	Bytes    int64  `json:"bytes"`
}

type Hook struct {
	Category string `json:"category"`
	Event    string `json:"event"`
	Stage    string `json:"stage"`
	Hook     string `json:"hook"`
	Ours     bool   `json:"ours,omitempty"`
}

type CronRef struct {
	Source   string `json:"source"`   // crontab | /etc/cron.d/<file>
	Schedule string `json:"schedule"` // the five time fields
	Command  string `json:"command"`  // first word of the command only
	Why      string `json:"why"`      // mentions the backup dir | script mentions the backup dir
}

type Disk struct {
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

// Overridable in tests.
var (
	CronDir     = "/etc/cron.d"
	ManageHooks = "/usr/local/cpanel/bin/manage_hooks"
	OurHook     = "/usr/local/servatum/whm-backup.sh"
)

type rawConfig struct {
	Config
	RetainDaily     flexInt `json:"backup_daily_retention"`
	RetainWeekly    flexInt `json:"backup_weekly_retention"`
	RetainMonthly   flexInt `json:"backup_monthly_retention"`
	KeepLocal       flexInt `json:"keeplocal"`
	DisableMetadata flexInt `json:"disable_metadata"`
}

// RunProbe gathers the facts. A source that fails is noted in Errors and
// the rest still runs.
func RunProbe(x Exec, now time.Time) Probe {
	p := Probe{Kind: "whm.probe.v1", At: now.UTC()}
	fail := func(what string, err error) { p.Errors = append(p.Errors, what+": "+err.Error()) }

	var cfg Config
	if d, err := call(x, "backup_config_get"); err != nil {
		fail("backup_config_get", err)
	} else {
		var w struct {
			C rawConfig `json:"backup_config"`
		}
		if err := json.Unmarshal(d, &w); err != nil {
			fail("backup_config_get", err)
		} else {
			cfg = w.C.Config
			if cfg.BackupDir == "" {
				cfg.BackupDir = "/backup"
			}
			c := w.C
			p.Config = ProbeConfig{Enabled: c.BackupEnable == 1, Dir: cfg.BackupDir, Type: c.BackupType,
				Daily: c.DailyEnable == 1, Days: c.BackupDays, Weekly: c.WeeklyEnable == 1, WeeklyDay: c.WeeklyDay,
				Monthly: c.MonthlyEnable == 1, MonthlyDates: c.MonthlyDates, RetainDaily: int(c.RetainDaily),
				RetainWeekly: int(c.RetainWeekly), RetainMonthly: int(c.RetainMonthly), KeepLocal: c.KeepLocal == 1,
				MetadataDisabled: c.DisableMetadata == 1}
		}
	}

	if d, err := call(x, "backup_destination_list"); err != nil {
		fail("backup_destination_list", err)
	} else {
		var w struct {
			L []struct {
				Name, Type string
				Disabled   flexInt
			} `json:"destination_list"`
		}
		if err := json.Unmarshal(d, &w); err != nil {
			fail("backup_destination_list", err)
		}
		for _, dst := range w.L {
			p.Destinations = append(p.Destinations, Destination{Name: dst.Name, Type: dst.Type, Disabled: dst.Disabled == 1})
		}
	}

	if accts, err := ListAccounts(x); err != nil {
		fail("listaccts", err)
	} else {
		p.Accounts.Total = len(accts)
		p.Accounts.Included = len(Expected(cfg, accts))
		for _, a := range accts {
			switch {
			case a.Backup != 1:
				p.Accounts.BackupOff = append(p.Accounts.BackupOff, a.User)
			case a.Suspended == 1 && cfg.BackupSuspendedAccounts != 1:
				p.Accounts.SuspendedExcluded++
			}
		}
	}

	if cfg.BackupDir != "" {
		p.LocalSets = FindSets(cfg.BackupDir)
		var st syscall.Statfs_t
		if err := syscall.Statfs(cfg.BackupDir, &st); err == nil {
			p.Disk = &Disk{TotalBytes: st.Blocks * uint64(st.Bsize), FreeBytes: st.Bavail * uint64(st.Bsize)}
		}
	}
	p.Logs = recentLogs(LogDir, 3)

	if out, err := x(ManageHooks, "list"); err != nil {
		fail("manage_hooks list", err)
	} else {
		p.Hooks = ParseHooks(out)
	}
	p.Cron = findCron(x, cfg.BackupDir)
	p.Hints = hints(p)
	return p
}

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// FindSets looks for backup sets (directories holding an accounts/
// directory) up to three levels under dir, instead of assuming a layout.
func FindSets(dir string) []LocalSet {
	var sets []LocalSet
	root := filepath.Clean(dir)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if depth := strings.Count(rel, string(filepath.Separator)); rel != "." && depth >= 3 {
			return filepath.SkipDir
		}
		if d.Name() != "accounts" || path == root {
			return nil
		}
		set := filepath.Dir(path)
		arch, _ := Scan([]string{set})
		ls := LocalSet{Path: set, Archives: len(arch), Tier: "other"}
		for _, a := range arch {
			ls.Bytes += a.Bytes
		}
		if base := filepath.Base(set); dateRE.MatchString(base) {
			ls.Date = base
			switch filepath.Base(filepath.Dir(set)) {
			case "weekly":
				ls.Tier = "weekly"
			case "monthly":
				ls.Tier = "monthly"
			default:
				if filepath.Dir(set) == root {
					ls.Tier = "daily"
				}
			}
		}
		sets = append(sets, ls)
		return filepath.SkipDir
	})
	sort.Slice(sets, func(i, j int) bool { return sets[i].Date > sets[j].Date })
	return sets
}

func recentLogs(dir string, n int) []LogState {
	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	var out []LogState
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			out = append(out, LogState{File: f, At: fi.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > n {
		out = out[:n]
	}
	for i := range out {
		out[i].State = finalState(out[i].File)
	}
	return out
}

// ParseHooks reads `manage_hooks list` text output and keeps the hooks on
// the events that matter for backups (System::Backup, PkgAcct::*).
func ParseHooks(out []byte) []Hook {
	var hooks []Hook
	var cat, ev string
	var cur *Hook
	flush := func() {
		if cur != nil && cur.Hook != "" && (cat == "PkgAcct" || (cat == "System" && ev == "Backup")) {
			cur.Ours = cur.Hook == OurHook
			hooks = append(hooks, *cur)
		}
		cur = nil
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		trim := strings.TrimSpace(line)
		switch {
		case trim == "":
			continue
		case !strings.HasPrefix(line, "\t") && strings.HasSuffix(trim, ":"):
			flush()
			cat, ev = strings.TrimSuffix(trim, ":"), ""
		case strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "\t\t") && strings.HasSuffix(trim, ":"):
			flush()
			ev = strings.TrimSuffix(trim, ":")
		case trim == "--":
			flush()
		default:
			k, v, ok := strings.Cut(trim, ":")
			if !ok {
				continue
			}
			if cur == nil {
				cur = &Hook{Category: cat, Event: ev}
			}
			switch strings.TrimSpace(k) {
			case "hook":
				cur.Hook = strings.TrimSpace(v)
			case "stage":
				cur.Stage = strings.TrimSpace(v)
			}
		}
	}
	flush()
	return hooks
}

// findCron lists cron entries that touch the backup dir: either the line
// names it, or the script it runs (first word, a readable text file under
// 1 MB) does. Only the schedule and the command path are kept.
func findCron(x Exec, backupDir string) []CronRef {
	if backupDir == "" {
		return nil
	}
	var refs []CronRef
	scan := func(source string, text []byte, hasUser bool) {
		sc := bufio.NewScanner(bytes.NewReader(text))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			f := strings.Fields(line)
			skip := 5
			if strings.HasPrefix(f[0], "@") {
				skip = 1
			}
			if hasUser {
				skip++
			}
			if len(f) <= skip || strings.Contains(f[0], "=") {
				continue
			}
			cmd := strings.Join(f[skip:], " ")
			bin := f[skip]
			if strings.Contains(cmd, "/usr/local/cpanel/bin/backup") {
				continue // cPanel's own backup run
			}
			why := ""
			if mentionsDir(cmd, backupDir) {
				why = "line mentions the backup dir"
			} else if scriptMentions(bin, backupDir) {
				why = "script mentions the backup dir"
			}
			if why != "" {
				refs = append(refs, CronRef{Source: source, Schedule: strings.Join(f[:skip-boolInt(hasUser)], " "), Command: bin, Why: why})
			}
		}
	}
	if out, err := x("crontab", "-l"); err == nil {
		scan("crontab", out, false)
	}
	files, _ := filepath.Glob(filepath.Join(CronDir, "*"))
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			scan(f, b, true)
		}
	}
	return refs
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mentionsDir(s, dir string) bool {
	d := strings.TrimRight(dir, "/")
	re := regexp.MustCompile(regexp.QuoteMeta(d) + `(["'/\s]|$)`)
	return re.MatchString(s)
}

func scriptMentions(path, dir string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	return mentionsDir(string(b), dir)
}

func hints(p Probe) []string {
	var h []string
	if !p.Config.Enabled && p.Config.Dir != "" {
		h = append(h, "cPanel backups are turned off")
	}
	lastOK := len(p.Logs) > 0 && p.Logs[0].State == "Success"
	if p.Config.KeepLocal && len(p.LocalSets) == 0 && lastOK {
		s := "local copies are kept by cPanel but none are on disk: something removes them after each run"
		var cmds []string
		for _, c := range p.Cron {
			cmds = append(cmds, c.Command)
		}
		if len(cmds) > 0 {
			s += " (likely: " + strings.Join(cmds, ", ") + ")"
		}
		h = append(h, s)
	}
	if p.Config.MetadataDisabled {
		h = append(h, "backup metadata is off, so per-account API checks are unavailable")
	}
	for _, k := range p.Hooks {
		if !k.Ours && k.Category == "System" && k.Event == "Backup" {
			h = append(h, "another System::Backup hook is registered: "+k.Hook)
		}
	}
	if p.Disk != nil && len(p.LocalSets) > 0 && uint64(p.LocalSets[0].Bytes) > p.Disk.FreeBytes {
		h = append(h, fmt.Sprintf("free space in %s (%d GB) is less than the latest set (%d GB)", p.Config.Dir, p.Disk.FreeBytes>>30, p.LocalSets[0].Bytes>>30))
	}
	if len(p.Logs) > 0 && p.Logs[0].State != "" && p.Logs[0].State != "Success" {
		h = append(h, "last run ended with Backup::"+p.Logs[0].State)
	}
	return h
}
