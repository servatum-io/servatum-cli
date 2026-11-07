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
	Problems          []Problem     `json:"problems,omitempty"`
	ProblemsTruncated int           `json:"problems_truncated,omitempty"`
	Verify            *VerifyResult `json:"verify,omitempty"`
}

type Result struct {
	Status  string // ok | warn | fail
	Message string
	Details Details
}

// Evaluate turns what was found into a status:
//
//	fail: no backup dir for the run, no archives, any expected account
//	      missing, or a sampled archive that does not open
//	warn: archives too small, or the sample check ran out of time
//	ok:   otherwise
func Evaluate(cfg Config, expected []string, dirs []string, archives map[string]Archive, v *VerifyResult) Result {
	d := Details{BackupType: cfg.BackupType, Dirs: dirs, AccountsExpected: len(expected), Archives: len(archives), Verify: v}
	var problems []Problem
	for _, u := range expected {
		if _, ok := archives[u]; !ok {
			problems = append(problems, Problem{User: u, Issue: "missing"})
		}
	}
	users := make([]string, 0, len(archives))
	for u := range archives {
		users = append(users, u)
	}
	sort.Strings(users)
	for _, u := range users {
		a := archives[u]
		d.TotalBytes += a.Bytes
		if a.Bytes < MinArchiveBytes {
			problems = append(problems, Problem{User: u, Issue: "too_small", Bytes: a.Bytes})
		}
	}
	if v != nil {
		for _, u := range v.Failed {
			problems = append(problems, Problem{User: u, Issue: "corrupt"})
		}
	}
	count := map[string]int{}
	for _, p := range problems {
		count[p.Issue]++
	}
	if len(problems) > MaxProblems {
		d.ProblemsTruncated = len(problems) - MaxProblems
		problems = problems[:MaxProblems]
	}
	d.Problems = problems

	r := Result{Status: "ok", Details: d}
	var why []string
	switch {
	case len(dirs) == 0:
		r.Status = "fail"
		why = append(why, "no backup directory for this run")
	case len(archives) == 0:
		r.Status = "fail"
		why = append(why, "no account archives")
	}
	if n := count["missing"]; n > 0 {
		r.Status = "fail"
		why = append(why, fmt.Sprintf("%d account(s) missing", n))
	}
	if n := count["corrupt"]; n > 0 {
		r.Status = "fail"
		why = append(why, fmt.Sprintf("%d sampled archive(s) do not open", n))
	}
	if n := count["too_small"]; n > 0 {
		if r.Status == "ok" {
			r.Status = "warn"
		}
		why = append(why, fmt.Sprintf("%d archive(s) under %d KB", n, MinArchiveBytes/1024))
	}
	if v != nil && v.Incomplete {
		if r.Status == "ok" {
			r.Status = "warn"
		}
		why = append(why, "sample check ran out of time")
	}
	if len(why) == 0 {
		r.Message = fmt.Sprintf("%d/%d accounts backed up", len(archives), len(expected))
		if v != nil && v.Sampled > 0 {
			r.Message += fmt.Sprintf(", %d sampled archive(s) open", v.OK)
		}
	} else {
		r.Message = strings.Join(why, "; ")
	}
	return r
}

func (d Details) JSON() json.RawMessage {
	b, _ := json.Marshal(d)
	return b
}
