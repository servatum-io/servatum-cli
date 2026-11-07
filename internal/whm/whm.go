// Package whm checks a cPanel & WHM backup run and builds a Servatum report.
//
// Sources, in order of trust:
//   - WHM API 1 (documented): backup_config_get (backup dir, type, schedule),
//     listaccts (users, per-account backup flag, suspended).
//   - The backup directory on disk. Its layout is not in cPanel's docs; the
//     default (<dir>/YYYY-MM-DD, <dir>/weekly/YYYY-MM-DD, <dir>/monthly/YYYY-MM-DD,
//     each with accounts/<user>.tar.gz|.tar|<user>/) was observed on a live
//     server and can be overridden with RunDirs.
//
// Everything here is read-only.
package whm

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// WHMAPI is the documented location of the whmapi1 binary.
const WHMAPI = "/usr/local/cpanel/bin/whmapi1"

// Exec runs a command and returns stdout. Swapped out in tests.
type Exec func(name string, args ...string) ([]byte, error)

func RealExec(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// flexInt accepts 1, "1", "" and null (WHM returns some numbers as strings).
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("not a number: %s", b)
	}
	*f = flexInt(n)
	return nil
}

type Config struct {
	BackupEnable            flexInt `json:"backupenable"`
	BackupDir               string  `json:"backupdir"`
	BackupType              string  `json:"backuptype"` // compressed | uncompressed | incremental
	DailyEnable             flexInt `json:"backup_daily_enable"`
	BackupDays              string  `json:"backupdays"` // 0=Sun … 6=Sat
	WeeklyEnable            flexInt `json:"backup_weekly_enable"`
	WeeklyDay               string  `json:"backup_weekly_day"`
	MonthlyEnable           flexInt `json:"backup_monthly_enable"`
	MonthlyDates            string  `json:"backup_monthly_dates"`
	BackupSuspendedAccounts flexInt `json:"backupsuspendedaccounts"`
}

type Account struct {
	User      string  `json:"user"`
	Backup    flexInt `json:"backup"`
	Suspended flexInt `json:"suspended"`
}

type envelope struct {
	Data     json.RawMessage `json:"data"`
	Metadata struct {
		Result flexInt `json:"result"`
		Reason string  `json:"reason"`
	} `json:"metadata"`
}

func call(x Exec, fn string, args ...string) (json.RawMessage, error) {
	out, err := x(WHMAPI, append([]string{"--output=json", fn}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("whmapi1 %s: %w", fn, err)
	}
	var e envelope
	if err := json.Unmarshal(out, &e); err != nil {
		return nil, fmt.Errorf("whmapi1 %s: bad JSON: %w", fn, err)
	}
	if e.Metadata.Result != 1 {
		return nil, fmt.Errorf("whmapi1 %s failed: %s", fn, e.Metadata.Reason)
	}
	return e.Data, nil
}

func GetConfig(x Exec) (Config, error) {
	d, err := call(x, "backup_config_get")
	if err != nil {
		return Config{}, err
	}
	var w struct {
		C Config `json:"backup_config"`
	}
	if err := json.Unmarshal(d, &w); err != nil {
		return Config{}, fmt.Errorf("backup_config_get: %w", err)
	}
	if w.C.BackupDir == "" {
		w.C.BackupDir = "/backup"
	}
	return w.C, nil
}

func ListAccounts(x Exec) ([]Account, error) {
	d, err := call(x, "listaccts", "want=user,backup,suspended")
	if err != nil {
		return nil, err
	}
	var w struct {
		A []Account `json:"acct"`
	}
	if err := json.Unmarshal(d, &w); err != nil {
		return nil, fmt.Errorf("listaccts: %w", err)
	}
	return w.A, nil
}

// Expected returns the users the backup should contain.
func Expected(cfg Config, accts []Account) []string {
	var out []string
	for _, a := range accts {
		if a.User == "" || a.Backup != 1 {
			continue
		}
		if a.Suspended == 1 && cfg.BackupSuspendedAccounts != 1 {
			continue
		}
		out = append(out, a.User)
	}
	return out
}
