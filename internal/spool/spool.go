// Package spool keeps run reports that could not be delivered and retries
// them later. Only transient failures are spooled; pings never are.
package spool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/servatum-io/servatum-cli/internal/api"
)

const (
	MaxAge   = 72 * time.Hour
	MaxFiles = 1000
	MaxBytes = 10 << 20
)

// Dir picks the spool directory: $SERVATUM_SPOOL, else /var/spool/servatum
// when writable (root), else ~/.local/state/servatum/spool.
func Dir() string {
	if d := os.Getenv("SERVATUM_SPOOL"); d != "" {
		return d
	}
	if err := os.MkdirAll("/var/spool/servatum", 0o700); err == nil && writable("/var/spool/servatum") {
		return "/var/spool/servatum"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "servatum-spool")
	}
	return filepath.Join(home, ".local", "state", "servatum", "spool")
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// KeyID is a short, non-reversible tag of an ingest key, so a flush only
// sends reports that were made with the same key. The key itself is never
// written to disk.
func KeyID(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])[:12]
}

// Save writes one report atomically (temp file + rename, mode 0600), then
// enforces the size caps by dropping the oldest reports.
func Save(dir, keyID string, r api.Run, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	key := unsafe.ReplaceAllString(r.IdempotencyKey, "_")
	if len(key) > 80 {
		key = key[:80]
	}
	name := fmt.Sprintf("%020d-%s-%s.json", now.UnixNano(), keyID, key)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	tmp.Close()
	os.Chmod(tmp.Name(), 0o600)
	final := filepath.Join(dir, name)
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	enforceCaps(dir)
	return final, nil
}

type entry struct {
	path  string
	at    time.Time
	keyID string
	bytes int64
}

func list(dir string) []entry {
	des, _ := os.ReadDir(dir)
	var out []entry
	for _, de := range des {
		n := de.Name()
		if de.IsDir() || !strings.HasSuffix(n, ".json") || strings.HasPrefix(n, ".") {
			continue
		}
		parts := strings.SplitN(n, "-", 3)
		if len(parts) < 3 {
			continue
		}
		ns, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, entry{filepath.Join(dir, n), time.Unix(0, ns), parts[1], info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

func enforceCaps(dir string) {
	es := list(dir)
	var total int64
	for _, e := range es {
		total += e.bytes
	}
	for len(es) > 0 && (len(es) > MaxFiles || total > MaxBytes) {
		os.Remove(es[0].path)
		total -= es[0].bytes
		es = es[1:]
	}
}

// Result summarises one flush.
type Result struct {
	Sent, Expired, Rejected, Left int
	Locked                        bool
}

// Flush sends spooled reports oldest-first. It stops at the first transient
// failure (the rest stay for next time), drops reports the API rejects
// permanently, and drops reports older than MaxAge. A second concurrent
// flush returns immediately with Locked=true.
func Flush(dir, keyID string, now time.Time, deadline time.Time, send func(api.Run) error) (Result, error) {
	var res Result
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	lf, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return res, err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		res.Locked = true
		return res, nil
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	var es []entry
	for _, e := range list(dir) {
		if now.Sub(e.at) > MaxAge {
			os.Remove(e.path) // expired, whatever key made it
			res.Expired++
			continue
		}
		if e.keyID == keyID {
			es = append(es, e)
		}
	}
	for i, e := range es {
		if !deadline.IsZero() && time.Now().After(deadline) {
			res.Left = len(es) - i
			return res, nil
		}
		b, err := os.ReadFile(e.path)
		var r api.Run
		if err != nil || json.Unmarshal(b, &r) != nil {
			os.Remove(e.path) // unreadable: nothing we can do with it
			res.Rejected++
			continue
		}
		err = send(r)
		if err == nil {
			os.Remove(e.path)
			res.Sent++
			continue
		}
		if !api.IsTransient(err) {
			os.Remove(e.path)
			res.Rejected++
			continue
		}
		res.Left = len(es) - i
		return res, err
	}
	return res, nil
}
