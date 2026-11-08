package whm

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// LogDir is where cPanel writes one log per backup run (cPanel support:
// backup and transporter log locations). Each run ends with a line
// "Final state is Backup::<State>".
var LogDir = "/usr/local/cpanel/logs/cpbackup"

var finalRE = regexp.MustCompile(`Final state is Backup::([A-Za-z]+)`)

type LogState struct {
	File  string    `json:"file"`
	State string    `json:"state,omitempty"` // Success, PartialFailure, Failure, …; "" = not written yet
	At    time.Time `json:"-"`               // log file mtime
}

// LatestLog returns the newest run log modified at or after since (zero
// since = any). ok is false when there is none.
func LatestLog(dir string, since time.Time) (LogState, bool) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	type fm struct {
		path string
		mt   time.Time
	}
	var fs []fm
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && !fi.IsDir() {
			fs = append(fs, fm{f, fi.ModTime()})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].mt.After(fs[j].mt) })
	for _, f := range fs {
		if !since.IsZero() && f.mt.Before(since) {
			break
		}
		return LogState{File: f.path, State: finalState(f.path), At: f.mt}, true
	}
	return LogState{}, false
}

func finalState(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	state := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if m := finalRE.FindStringSubmatch(sc.Text()); m != nil {
			state = m[1]
		}
	}
	return state
}

// WaitFinal polls the newest log since `since` until it has a final state or
// the deadline passes. sleep is swapped out in tests.
func WaitFinal(dir string, since, deadline time.Time, every time.Duration, now func() time.Time, sleep func(time.Duration)) (LogState, bool) {
	for {
		if ls, ok := LatestLog(dir, since); ok && ls.State != "" {
			return ls, true
		}
		if !now().Before(deadline) {
			ls, _ := LatestLog(dir, since)
			return ls, false
		}
		sleep(every)
	}
}
