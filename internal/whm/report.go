package whm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const Kind = "whm.backup.v1"

// MinArchiveBytes: a cPanel account archive smaller than this is flagged
// too_small (it holds at least the account's config files).
const MinArchiveBytes = 10 * 1024

// MaxProblems bounds the problem list so the report stays under the API's
// 16 KB body limit (a full per-account list is a later, separate change).
const MaxProblems = 100

type Problem struct {
	User  string `json:"user"`
	Issue string `json:"issue"` // missing | too_small | corrupt
	Bytes int64  `json:"bytes,omitempty"`
}

type Details struct {
	BackupType        string        `json:"backup_type"`
	Dirs              []string      `json:"dirs"`
	AccountsExpected  int           `json:"accounts_expected"`
	Archives          int           `json:"archives"`
	TotalBytes        int64         `json:"total_bytes"`
	Missing           int           `json:"missing,omitempty"`
	MissingFrom       string        `json:"missing_from,omitempty"` // files | api
	Problems          []Problem     `json:"problems,omitempty"`
	ProblemsTruncated int           `json:"problems_truncated,omitempty"`
	Verify            *VerifyResult `json:"verify,omitempty"`
	FinalState        string        `json:"final_state,omitempty"` // from the cPanel run log
	FinalStateWaited  bool          `json:"final_state_waited,omitempty"`
}

type Result struct {
	Status  string // ok | warn | fail
	Message string
	Details Details
}

// Input is everything one check gathered. Zero values mean "not available".
type Input struct {
	Cfg        Config
	Expected   []string
	Dirs       []string
	Archives   map[string]Archive
	Verify     *VerifyResult
	UserStatus map[string]string // backup_user_list; only used when no local set exists
	FinalState string            // "" = unknown
	WaitedLog  bool              // a final state was waited for (and may be missing)
}

// Evaluate turns what was found into a status.
//
//	fail: cPanel's final state is not Success; no backup at all (no local set
//	      and no API answer); expected accounts missing; a sampled archive
//	      that does not open
//	warn: archives too small; the sample check ran out of time; no final
//	      state appeared in the log while we waited
//	ok:   otherwise
//
// Missing accounts come from the local set when there is one (it is read
// before any post-processing can move it), else from backup_user_list.
// When every expected account is missing, the run is reported as one
// problem instead of one line per account.
func Evaluate(in Input) Result {
	d := Details{BackupType: in.Cfg.BackupType, Dirs: in.Dirs, AccountsExpected: len(in.Expected),
		Archives: len(in.Archives), Verify: in.Verify, FinalState: in.FinalState, FinalStateWaited: in.WaitedLog}
	var missing []string
	switch {
	case len(in.Dirs) > 0:
		d.MissingFrom = "files"
		for _, u := range in.Expected {
			if _, ok := in.Archives[u]; !ok {
				missing = append(missing, u)
			}
		}
	case in.UserStatus != nil:
		d.MissingFrom = "api"
		for _, u := range in.Expected {
			if in.UserStatus[u] != "active" {
				missing = append(missing, u)
			}
		}
	}
	d.Missing = len(missing)
	allMissing := len(in.Expected) > 0 && len(missing) == len(in.Expected)

	var problems []Problem
	if !allMissing {
		for _, u := range missing {
			problems = append(problems, Problem{User: u, Issue: "missing"})
		}
	}
	users := make([]string, 0, len(in.Archives))
	for u := range in.Archives {
		users = append(users, u)
	}
	sort.Strings(users)
	tooSmall := 0
	for _, u := range users {
		a := in.Archives[u]
		d.TotalBytes += a.Bytes
		if a.Bytes < MinArchiveBytes {
			tooSmall++
			problems = append(problems, Problem{User: u, Issue: "too_small", Bytes: a.Bytes})
		}
	}
	corrupt := 0
	if in.Verify != nil {
		for _, u := range in.Verify.Failed {
			corrupt++
			problems = append(problems, Problem{User: u, Issue: "corrupt"})
		}
	}
	if len(problems) > MaxProblems {
		d.ProblemsTruncated = len(problems) - MaxProblems
		problems = problems[:MaxProblems]
	}
	d.Problems = problems

	r := Result{Status: "ok", Details: d}
	var why []string
	fail := func(s string) { r.Status = "fail"; why = append(why, s) }
	warn := func(s string) {
		if r.Status == "ok" {
			r.Status = "warn"
		}
		why = append(why, s)
	}
	if in.FinalState != "" && in.FinalState != "Success" {
		fail("cPanel final state: Backup::" + in.FinalState)
	}
	switch {
	case len(in.Dirs) == 0 && in.UserStatus == nil:
		fail("no backup found for this run")
	case allMissing:
		fail("no account was backed up")
	case len(missing) > 0:
		fail(fmt.Sprintf("%d account(s) missing", len(missing)))
	}
	if corrupt > 0 {
		fail(fmt.Sprintf("%d sampled archive(s) do not open", corrupt))
	}
	if tooSmall > 0 {
		warn(fmt.Sprintf("%d archive(s) under %d KB", tooSmall, MinArchiveBytes/1024))
	}
	if in.Verify != nil && in.Verify.Incomplete {
		warn("sample check ran out of time")
	}
	if in.WaitedLog && in.FinalState == "" {
		warn("cPanel's backup log had no final state when we stopped waiting")
	}
	if len(why) > 0 {
		r.Message = strings.Join(why, "; ")
		return r
	}
	backed := len(in.Expected) - len(missing)
	r.Message = fmt.Sprintf("%d/%d accounts backed up", backed, len(in.Expected))
	if len(in.Dirs) == 0 {
		r.Message += " (no local copy to check)"
	}
	if in.Verify != nil && in.Verify.Sampled > 0 {
		r.Message += fmt.Sprintf(", %d sampled archive(s) open", in.Verify.OK)
	}
	if in.FinalState == "Success" {
		r.Message += ", cPanel: Success"
	}
	return r
}

func (d Details) JSON() json.RawMessage {
	b, _ := json.Marshal(d)
	return b
}
