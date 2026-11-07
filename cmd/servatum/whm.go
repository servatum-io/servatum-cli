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
	"path/filepath"
	"strings"
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
)

func whmStateFile() string {
	if d := os.Getenv("SERVATUM_STATE"); d != "" {
		return filepath.Join(d, "whm-start")
	}
	return "/var/lib/servatum/whm-start"
}

func cmdWHM(args []string, stdin io.Reader, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: servatum whm start|report|hook [flags]")
		return 2
	}
	switch args[0] {
	case "start":
		return whmStart(args[1:], stderr)
	case "report":
		return whmReport(args[1:], stderr)
	case "hook":
		return whmHook(args[1:], stdin, stderr)
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
	sample := fs.Int("sample", 3, "archives to test-read (0 = off)")
	maxPct := fs.Float64("sample-max-pct", 5, "never test-read more than this % of archives (min 1)")
	vtimeout := fs.Duration("verify-timeout", 120*time.Second, "time cap for the test-read pass")
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

	res, err := whmCheck(runDay, *sample, *maxPct, *vtimeout)
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

func whmCheck(day time.Time, sample int, maxPct float64, vtimeout time.Duration) (whm.Result, error) {
	cfg, err := whm.GetConfig(whmExec)
	if err != nil {
		return whm.Result{}, err
	}
	accts, err := whm.ListAccounts(whmExec)
	if err != nil {
		return whm.Result{}, err
	}
	dirs := whm.RunDirs(cfg.BackupDir, day)
	archives, err := whm.Scan(dirs)
	if err != nil {
		return whm.Result{}, err
	}
	var v *whm.VerifyResult
	if sample > 0 && len(archives) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), vtimeout)
		defer cancel()
		vr := whm.Verify(ctx, archives, sample, maxPct, rand.New(rand.NewSource(whmNow().UnixNano())))
		v = &vr
	}
	return whm.Evaluate(cfg, whm.Expected(cfg, accts), dirs, archives, v), nil
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
		return whmReport(args, stderr)
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
	fmt.Fprintf(stderr, "servatum: WHM hook installed (%s, System::Backup pre+post, dry-run=%v)\n", hookPath(), *dry)
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
