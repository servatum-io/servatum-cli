// servatum — report backup job results to Servatum.
//
//	servatum run  --job job_… [flags] -- <command> [args…]
//	servatum ping [--start|--fail] [--log FILE|-] <ping_token>
//	servatum version
//
// Environment: SERVATUM_KEY, SERVATUM_JOB, SERVATUM_ENDPOINT.
//
// A reporting failure never changes the wrapped command's exit code: the
// backup script behaves exactly as it would without servatum.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/servatum-io/servatum-cli/internal/api"
	"github.com/servatum-io/servatum-cli/internal/runner"
	"github.com/servatum-io/servatum-cli/internal/spool"
)

var version = "0.1.0-dev"

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "ping":
		return cmdPing(args[1:], stderr)
	case "flush":
		return cmdFlush(args[1:], stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "servatum", version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "servatum: unknown command %q\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  servatum run  --job job_… [flags] -- <command> [args…]
  servatum ping [--start|--fail] [--log FILE|-] <ping_token>
  servatum flush                 send reports spooled while Servatum was unreachable
  servatum version

Environment: SERVATUM_KEY (ingest key), SERVATUM_JOB, SERVATUM_ENDPOINT, SERVATUM_SPOOL
Run "servatum run -h" or "servatum ping -h" for flags.
`)
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	job := fs.String("job", os.Getenv("SERVATUM_JOB"), "job ID (job_…); default $SERVATUM_JOB")
	key := fs.String("key", "", "ingest key (sv_ing_…); default $SERVATUM_KEY — prefer the env var, flags show up in ps")
	endpoint := fs.String("endpoint", envOr("SERVATUM_ENDPOINT", api.DefaultEndpoint), "API base URL")
	warnCodes := fs.String("warn-exit-codes", "", "comma-separated exit codes reported as warn instead of fail, e.g. 24 for rsync")
	noStart := fs.Bool("no-start", false, "skip the 'started' report sent before the command runs")
	message := fs.String("message", "", "message to send with the report")
	sendOutput := fs.Bool("send-output", false, "include the last 8 KB of the command's output in the report (off by default: output may contain paths or filenames)")
	size := fs.Int64("size-bytes", 0, "backup size in bytes, if known")
	snapshot := fs.String("snapshot-id", "", "snapshot / archive ID, if known")
	idem := fs.String("idempotency-key", "", "idempotency key; default derived from job, host and start time")
	dry := fs.Bool("dry-run", false, "print the reports as JSON instead of sending them")
	quiet := fs.Bool("quiet", false, "don't print servatum's own status lines")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "servatum run: missing command after --")
		return 2
	}
	if *key == "" {
		*key = os.Getenv("SERVATUM_KEY")
	}
	warn, err := parseCodes(*warnCodes)
	if err != nil {
		fmt.Fprintln(stderr, "servatum run: --warn-exit-codes:", err)
		return 2
	}

	logf := func(format string, a ...any) {
		if !*quiet {
			fmt.Fprintf(stderr, "servatum: "+format+"\n", a...)
		}
	}
	client := api.New(*endpoint, *key, version)
	send := func(r api.Run) {
		if *dry {
			b, _ := json.MarshalIndent(r, "", "  ")
			fmt.Fprintln(stderr, string(b))
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var resp *api.RunResponse
		err := api.Retry(ctx, func(c context.Context) error {
			var e error
			resp, e = client.PostRun(c, r)
			return e
		})
		if err == nil {
			logf("reported %s for %s (run %s, state %s)", r.Status, r.Job, orDash(resp.RunID), orDash(resp.State))
			return
		}
		if !api.IsTransient(err) {
			logf("WARNING report %s for %s rejected, not retried: %v", r.Status, r.Job, err)
			return
		}
		if r.Status == api.StatusStarted {
			// A late "started" says nothing the final report won't; don't spool it.
			logf("WARNING could not report started for %s: %v", r.Job, err)
			return
		}
		if path, serr := spool.Save(spool.Dir(), spool.KeyID(client.Key), r, time.Now()); serr == nil {
			logf("WARNING could not reach Servatum (%v); report spooled to %s, retried for 72 h", err, path)
		} else {
			logf("WARNING could not report %s for %s: %v (spool failed: %v)", r.Status, r.Job, err, serr)
		}
	}

	host, _ := os.Hostname()
	start := time.Now().UTC()
	base := *idem
	if base == "" {
		base = fmt.Sprintf("%s-%s-%d", *job, host, start.UnixNano())
	}
	// Bad config is reported up front but the backup still runs.
	if *key == "" && !*dry {
		logf("WARNING no ingest key (SERVATUM_KEY); the command runs but nothing is reported")
	}
	if *job == "" && !*dry {
		logf("WARNING no job (SERVATUM_JOB / --job); the command runs but nothing is reported")
	}
	canSend := *dry || (*key != "" && *job != "")

	if canSend && !*dry {
		flushSpool(client, logf, 10*time.Second)
	}
	if !*noStart && canSend {
		send(api.Run{Job: *job, Status: api.StatusStarted, StartedAt: ts(start), IdempotencyKey: base + "-start"})
	}

	res := runner.Run(argv, stdout, stderr)

	status := api.StatusOK
	if res.ExitCode != 0 {
		status = api.StatusFail
		if warn[res.ExitCode] {
			status = api.StatusWarn
		}
	}
	msg := *message
	if *sendOutput {
		if msg != "" {
			msg += "\n"
		}
		msg += res.Output
	} else if msg == "" && res.Err != nil {
		msg = res.Err.Error() // e.g. command not found; no customer output involved
	}
	code := res.ExitCode
	if canSend {
		send(api.Run{
			Job:            *job,
			Status:         status,
			StartedAt:      ts(res.StartedAt),
			FinishedAt:     ts(res.FinishedAt),
			ExitCode:       &code,
			SizeBytes:      *size,
			SnapshotID:     *snapshot,
			Message:        msg,
			IdempotencyKey: base,
		})
	}
	if res.Err != nil {
		fmt.Fprintf(stderr, "servatum run: %v\n", res.Err)
	}
	return res.ExitCode
}

func cmdPing(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	fs.SetOutput(stderr)
	start := fs.Bool("start", false, "signal the job has started")
	fail := fs.Bool("fail", false, "signal the job failed")
	logPath := fs.String("log", "", "file to send as the run log (- for stdin); last 10 KB kept")
	endpoint := fs.String("endpoint", envOr("SERVATUM_ENDPOINT", api.DefaultEndpoint), "API base URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "servatum ping: need exactly one ping token")
		return 2
	}
	if *start && *fail {
		fmt.Fprintln(stderr, "servatum ping: --start and --fail are exclusive")
		return 2
	}
	status := ""
	if *start {
		status = "start"
	} else if *fail {
		status = "fail"
	}
	var log string
	if *logPath != "" {
		var b []byte
		var err error
		if *logPath == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(*logPath)
		}
		if err != nil {
			fmt.Fprintln(stderr, "servatum ping: --log:", err)
			return 2
		}
		log = string(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pc := api.New(*endpoint, "", version)
	if err := api.Retry(ctx, func(c context.Context) error { return pc.Ping(c, fs.Arg(0), status, log) }); err != nil {
		fmt.Fprintln(stderr, "servatum ping:", err)
		var ae *api.APIError
		if errors.As(err, &ae) && !ae.Retryable() {
			return 3 // permanent: bad token etc.
		}
		return 1
	}
	return 0
}

func parseCodes(s string) (map[int]bool, error) {
	m := map[int]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 255 {
			return nil, fmt.Errorf("bad exit code %q", p)
		}
		m[n] = true
	}
	return m, nil
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// flushSpool sends spooled reports, best effort, within budget.
func flushSpool(client *api.Client, logf func(string, ...any), budget time.Duration) spool.Result {
	dir := spool.Dir()
	res, err := spool.Flush(dir, spool.KeyID(client.Key), time.Now(), time.Now().Add(budget), func(r api.Run) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, e := client.PostRun(ctx, r)
		return e
	})
	if res.Sent+res.Expired+res.Rejected > 0 || err != nil {
		logf("spool %s: sent %d, expired %d, rejected %d, left %d", dir, res.Sent, res.Expired, res.Rejected, res.Left)
	}
	if err != nil {
		logf("WARNING spool flush stopped: %v", err)
	}
	return res
}

func cmdFlush(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("flush", flag.ContinueOnError)
	fs.SetOutput(stderr)
	key := fs.String("key", "", "ingest key; default $SERVATUM_KEY")
	endpoint := fs.String("endpoint", envOr("SERVATUM_ENDPOINT", api.DefaultEndpoint), "API base URL")
	quiet := fs.Bool("quiet", false, "only print problems")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *key == "" {
		*key = os.Getenv("SERVATUM_KEY")
	}
	if *key == "" {
		fmt.Fprintln(stderr, "servatum flush: no ingest key (SERVATUM_KEY)")
		return 2
	}
	logf := func(format string, a ...any) {
		if !*quiet || strings.HasPrefix(format, "WARNING") {
			fmt.Fprintf(stderr, "servatum: "+format+"\n", a...)
		}
	}
	res := flushSpool(api.New(*endpoint, *key, version), logf, 60*time.Second)
	if res.Locked {
		logf("another flush is running")
		return 0
	}
	if res.Left > 0 {
		return 1
	}
	return 0
}
