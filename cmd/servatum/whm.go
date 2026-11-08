package main

// WHM backup integration (P5).
//
//	servatum whm start   [--job …] [--dry-run]          record start, send "started"
//	servatum whm report  [--job …] [--dry-run] [flags]  check the run, send the result
//	servatum whm hook    [--dry-run]                    entry point for the cPanel hook
//	servatum install whm   --job … [--dry-run]          register System::Backup pre/post
//	servatum uninstall whm                              remove both hooks
//
// The hook runs as root via cPanel's Standardized Hooks (System::Backup,
// stages pre and post; no data is passed, it cannot block the backup).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/servatum-io/servatum-cli/internal/api"
	"github.com/servatum-io/servatum-cli/internal/whm"
)

// Overridable in tests.
var (
	whmExec     whm.Exec  = whm.RealExec
	manageHooks           = "/usr/local/cpanel/bin/manage_hooks"
	whmPrefix             = "/usr/local/servatum"
	whmEnvFile            = "/etc/servatum/env"
	whmLogFile            = "/var/log/servatum-whm.log"
	whmNow                = time.Now
	whmStdin    io.Reader = os.Stdin
	whmSleep              = time.Sleep
	whmStdout   io.Writer = os.Stdout
	whmSpawn              = spawnDetached
)

// spawnDetached starts this binary again in its own session, logging to
// whmLogFile, and does not wait for it.
func spawnDetached(args []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(whmLogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := exec.Command(self, args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func whmStateFile() string {
	if d := os.Getenv("SERVATUM_STATE"); d != "" {
		return filepath.Join(d, "whm-start")
	}
	return "/var/lib/servatum/whm-start"
}

func cmdWHM(args []string, stdin io.Reader, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: servatum whm start|report|hook|probe [flags]")
		return 2
	}
	switch args[0] {
	case "start":
		return whmStart(args[1:], stderr)
	case "report":
		return whmReport(args[1:], stderr)
	case "hook":
		return whmHook(args[1:], stdin, stderr)
	case "probe":
		return whmProbe(args[1:], stderr)
	}
	fmt.Fprintf(stderr, "servatum whm: unknown command %q\n", args[0])
	return 2
}

type whmCommon struct {
	job, key, endpoint *string
	dry, quiet         *bool
}

func whmFlags(fs *flag.FlagSet) whmCommon {
	return whmCommon{
		job:      fs.String("job", os.Getenv("SERVATUM_JOB"), "job ID; default $SERVATUM_JOB"),
		key:      fs.String("key", "", "ingest key; default $SERVATUM_KEY"),
		endpoint: fs.String("endpoint", envOr("SERVATUM_ENDPOINT", api.DefaultEndpoint), "API base URL"),
		dry:      fs.Bool("dry-run", os.Getenv("SERVATUM_DRY_RUN") == "1", "print reports instead of sending (default $SERVATUM_DRY_RUN=1)"),
		quiet:    fs.Bool("quiet", false, "only print problems"),
	}
}

func (c whmCommon) client(stderr io.Writer) (*api.Client, func(string, ...any), bool) {
	if *c.key == "" {
		*c.key = os.Getenv("SERVATUM_KEY")
	}
	logf := func(format string, a ...any) {
		if !*c.quiet || strings.HasPrefix(format, "WARNING") {
			fmt.Fprintf(stderr, "servatum: "+format+"\n", a...)
		}
	}
	ok := *c.dry || (*c.key != "" && *c.job != "")
	if !ok {
		logf("WARNING need SERVATUM_KEY and a job (--job / SERVATUM_JOB); nothing reported")
	}
	return api.New(*c.endpoint, *c.key, version), logf, ok
}

func whmStart(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("whm start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := whmFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	now := whmNow().UTC()
	if err := os.MkdirAll(filepath.Dir(whmStateFile()), 0o700); err == nil {
		_ = os.WriteFile(whmStateFile(), []byte(now.Format(time.RFC3339)), 0o600)
	}
	client, logf, ok := c.client(stderr)
	if !ok {
		return 0
	}
	host, _ := os.Hostname()
	r := api.Run{Job: *c.job, Status: api.StatusStarted, StartedAt: ts(now), Kind: whm.Kind,
		IdempotencyKey: fmt.Sprintf("%s-%s-%d-start", *c.job, host, now.Unix())}
	if !*c.dry {
		flushSpool(client, logf, 10*time.Second)
	}
	deliver(client, r, *c.dry, stderr, logf)
	return 0
}

func whmReport(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("whm report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := whmFlags(fs)
	sample := fs.Int("sample", 3, "archives to test-read at least (0 with --sample-pct 0 = off)")
	pct := fs.Float64("sample-pct", 5, "test-read at least this % of archives (rounded up)")
	vtimeout := fs.Duration("verify-timeout", 120*time.Second, "time cap for the test-read pass")
	waitLog := fs.Duration("wait-log", 0, "wait up to this long for cPanel's run log to show its final state (the hook uses 10m)")
	day := fs.String("day", "", "backup date YYYY-MM-DD; default the recorded start date, else today")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client, logf, ok := c.client(stderr)
	now := whmNow()

	var started time.Time
	if b, err := os.ReadFile(whmStateFile()); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil && now.Sub(t) < 24*time.Hour {
			started = t
		}
	}
	runDay := now
	if !started.IsZero() {
		runDay = started.Local()
	}
	if *day != "" {
		t, err := time.ParseInLocation("2006-01-02", *day, time.Local)
		if err != nil {
			fmt.Fprintln(stderr, "servatum whm report: --day wants YYYY-MM-DD")
			return 2
		}
		runDay = t
	}

	r := api.Run{Job: *c.job, Kind: whm.Kind, FinishedAt: ts(now)}
	if !started.IsZero() {
		r.StartedAt = ts(started)
	}
	host, _ := os.Hostname()
	keyTime := now
	if !started.IsZero() {
		keyTime = started
	}
	r.IdempotencyKey = fmt.Sprintf("%s-%s-%d", *c.job, host, keyTime.Unix())

	since := now.Add(-24 * time.Hour)
	if !started.IsZero() {
		since = started.Add(-time.Minute)
	}
	res, err := whmCheck(runDay, since, *sample, *pct, *vtimeout, *waitLog)
	if err != nil {
		r.Status, r.Message = api.StatusFail, "could not read the WHM backup state: "+err.Error()
	} else {
		r.Status, r.Message = res.Status, res.Message
		r.SizeBytes = res.Details.TotalBytes
		r.Details = res.Details.JSON()
	}
	logf("whm backup %s: %s", r.Status, r.Message)
	if !ok {
		return 0
	}
	if !*c.dry {
		flushSpool(client, logf, 10*time.Second)
	}
	deliver(client, r, *c.dry, stderr, logf)
	return 0
}

// whmCheck reads the run's archives first (before anything can move them),
// then, if asked, waits for cPanel's final state, and only then asks the API
// about accounts when no local set exists.
func whmCheck(day, since time.Time, sample int, pct float64, vtimeout, waitLog time.Duration) (whm.Result, error) {
	cfg, err := whm.GetConfig(whmExec)
	if err != nil {
		return whm.Result{}, err
	}
	accts, err := whm.ListAccounts(whmExec)
	if err != nil {
		return whm.Result{}, err
	}
	in := whm.Input{Cfg: cfg, Expected: whm.Expected(cfg, accts)}
	in.Dirs = whm.RunDirs(cfg.BackupDir, day)
	if in.Archives, err = whm.Scan(in.Dirs); err != nil {
		return whm.Result{}, err
	}
	if (sample > 0 || pct > 0) && len(in.Archives) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), vtimeout)
		vr := whm.Verify(ctx, in.Archives, sample, pct, rand.New(rand.NewSource(whmNow().UnixNano())))
		cancel()
		in.Verify = &vr
	}
	var ls whm.LogState
	if waitLog > 0 {
		in.WaitedLog = true
		ls, _ = whm.WaitFinal(whm.LogDir, since, whmNow().Add(waitLog), 15*time.Second, whmNow, whmSleep)
	} else {
		ls, _ = whm.LatestLog(whm.LogDir, since)
	}
	in.FinalState = ls.State
	if len(in.Dirs) == 0 {
		if us, err := whm.BackupUserList(whmExec, day.Format("2006-01-02")); err == nil {
			in.UserStatus = us
		}
	}
	return whm.Evaluate(in), nil
}

// whmHook is what cPanel runs. It reads the event JSON from stdin (the docs
// require reading all of it) and dispatches on the stage.
func whmHook(args []string, stdin io.Reader, stderr io.Writer) int {
	raw, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	var ev struct {
		Context struct {
			Stage, Category, Event string
		} `json:"context"`
		Hook struct{ Stage string } `json:"hook"`
	}
	_ = json.Unmarshal(raw, &ev)
	stage := ev.Hook.Stage
	if stage == "" {
		stage = ev.Context.Stage
	}
	fmt.Fprintf(stderr, "servatum: %s whm hook stage=%q\n", whmNow().Format(time.RFC3339), stage)
	switch stage {
	case "pre":
		return whmStart(args, stderr)
	case "post":
		// cPanel writes the run's final state only after its post hooks
		// return, so the check runs detached and waits for it.
		if err := whmSpawn(append([]string{"whm", "report", "--wait-log", "10m"}, args...)); err != nil {
			fmt.Fprintf(stderr, "servatum: WARNING could not start the detached check (%v); checking now without the final state\n", err)
			return whmReport(args, stderr)
		}
		fmt.Fprintln(stderr, "servatum: post-backup check started in the background")
		return 0
	}
	fmt.Fprintf(stderr, "servatum: WARNING unknown hook stage %q; nothing done\n", stage)
	return 0
}

const hookScript = `#!/bin/sh
# Servatum WHM backup hook (cPanel System::Backup, stages pre + post).
# Installed by "servatum install whm"; remove with "servatum uninstall whm".
# Always exits 0: a reporting problem never affects the backup.
if [ -r %[2]s ]; then set -a; . %[2]s; set +a; fi
timeout 300 %[1]s/servatum whm hook >>%[3]s 2>&1
exit 0
`

func hookPath() string { return filepath.Join(whmPrefix, "whm-backup.sh") }

func hookArgs(op, stage string) []string {
	return []string{op, "script", hookPath(), "--manual", "--category", "System", "--event", "Backup", "--stage=" + stage}
}

func cmdInstall(args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "whm" {
		fmt.Fprintln(stderr, "usage: servatum install whm --job job_… [--dry-run]")
		return 2
	}
	fs := flag.NewFlagSet("install whm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	job := fs.String("job", os.Getenv("SERVATUM_JOB"), "job ID to report to")
	dry := fs.Bool("dry-run", false, "hook logs reports to "+whmLogFile+" instead of sending them")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *job == "" && !*dry {
		fmt.Fprintln(stderr, "servatum install whm: --job is required (or --dry-run)")
		return 2
	}
	if err := os.MkdirAll(whmPrefix, 0o755); err != nil {
		fmt.Fprintln(stderr, "servatum install whm:", err)
		return 1
	}
	if err := os.WriteFile(hookPath(), []byte(fmt.Sprintf(hookScript, whmPrefix, whmEnvFile, whmLogFile)), 0o700); err != nil {
		fmt.Fprintln(stderr, "servatum install whm:", err)
		return 1
	}
	set := map[string]string{"SERVATUM_DRY_RUN": "0"}
	if *dry {
		set["SERVATUM_DRY_RUN"] = "1"
	}
	if *job != "" {
		set["SERVATUM_JOB"] = *job
	}
	if k := os.Getenv("SERVATUM_KEY"); k != "" {
		set["SERVATUM_KEY"] = k
	}
	if err := writeEnv(whmEnvFile, set); err != nil {
		fmt.Fprintln(stderr, "servatum install whm:", err)
		return 1
	}
	for _, st := range []string{"pre", "post"} {
		_, _ = whmExec(manageHooks, hookArgs("delete", st)...) // make re-runs idempotent
		if out, err := whmExec(manageHooks, hookArgs("add", st)...); err != nil {
			fmt.Fprintf(stderr, "servatum install whm: manage_hooks add %s: %v %s\n", st, err, out)
			return 1
		}
	}
	fmt.Fprintf(stderr, "servatum: WHM hook installed (%s, System::Backup pre+post, dry-run=%v)\n\n", hookPath(), *dry)
	printProbe(stderr, whm.RunProbe(whmExec, whmNow()))
	return 0
}

func cmdUninstall(args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "whm" {
		fmt.Fprintln(stderr, "usage: servatum uninstall whm")
		return 2
	}
	rc := 0
	for _, st := range []string{"pre", "post"} {
		if out, err := whmExec(manageHooks, hookArgs("delete", st)...); err != nil {
			fmt.Fprintf(stderr, "servatum uninstall whm: manage_hooks delete %s: %v %s\n", st, err, out)
			rc = 1
		}
	}
	if err := os.Remove(hookPath()); err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(stderr, "servatum uninstall whm:", err)
		rc = 1
	}
	fmt.Fprintln(stderr, "servatum: WHM hook removed (env file and log kept)")
	return rc
}

// writeEnv sets KEY=value lines in an env file, keeping other lines; mode 600.
func writeEnv(path string, set map[string]string) error {
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			k, _, _ := strings.Cut(l, "=")
			if _, replace := set[strings.TrimSpace(k)]; !replace && l != "" {
				lines = append(lines, l)
			}
		}
	}
	for _, k := range []string{"SERVATUM_KEY", "SERVATUM_JOB", "SERVATUM_DRY_RUN"} {
		if v, ok := set[k]; ok {
			lines = append(lines, k+"="+v)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func whmProbe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("whm probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the facts as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p := whm.RunProbe(whmExec, whmNow())
	if *asJSON {
		b, _ := json.MarshalIndent(p, "", "  ")
		fmt.Fprintln(whmStdout, string(b))
		return 0
	}
	printProbe(whmStdout, p)
	return 0
}

// printProbe is the "here is what we found" summary the wizard will show.
func printProbe(w io.Writer, p whm.Probe) {
	c := p.Config
	on := map[bool]string{true: "on", false: "off"}
	fmt.Fprintln(w, "What this server's cPanel backup looks like:")
	fmt.Fprintf(w, "  Backups:       %s, %s, in %s\n", on[c.Enabled], orDash(c.Type), orDash(c.Dir))
	fmt.Fprintf(w, "  Schedule:      daily %s (days %s), weekly %s, monthly %s\n", on[c.Daily], orDash(c.Days), on[c.Weekly], on[c.Monthly])
	fmt.Fprintf(w, "  Keep local:    %s, retain %d daily\n", on[c.KeepLocal], c.RetainDaily)
	fmt.Fprintf(w, "  Accounts:      %d of %d included", p.Accounts.Included, p.Accounts.Total)
	if len(p.Accounts.BackupOff) > 0 {
		fmt.Fprintf(w, " (backups off: %s)", strings.Join(p.Accounts.BackupOff, ", "))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Destinations:  %d remote\n", len(p.Destinations))
	if len(p.LocalSets) == 0 {
		fmt.Fprintln(w, "  Local sets:    none on disk")
	} else {
		ls := p.LocalSets[0]
		fmt.Fprintf(w, "  Local sets:    %d, latest %s (%s, %d archives, %.1f GB)\n", len(p.LocalSets), ls.Path, ls.Tier, ls.Archives, float64(ls.Bytes)/(1<<30))
	}
	if len(p.Logs) > 0 {
		fmt.Fprintf(w, "  Last run:      %s, %s\n", p.Logs[0].At.Local().Format("2006-01-02 15:04"), orDash(p.Logs[0].State))
	}
	for _, k := range p.Hooks {
		if !k.Ours {
			fmt.Fprintf(w, "  Other hook:    %s::%s %s → %s\n", k.Category, k.Event, k.Stage, k.Hook)
		}
	}
	for _, r := range p.Cron {
		fmt.Fprintf(w, "  Cron:          %s %s (%s)\n", r.Schedule, r.Command, r.Why)
	}
	if p.Disk != nil {
		fmt.Fprintf(w, "  Free space:    %.0f GB of %.0f GB\n", float64(p.Disk.FreeBytes)/(1<<30), float64(p.Disk.TotalBytes)/(1<<30))
	}
	for _, h := range p.Hints {
		fmt.Fprintln(w, "  ! "+h)
	}
	for _, e := range p.Errors {
		fmt.Fprintln(w, "  could not read "+e)
	}
}
