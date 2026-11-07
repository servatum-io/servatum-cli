package whm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RunDirs returns the backup directories a run started on day may have
// written: daily, weekly and monthly sets for that date, if they exist.
func RunDirs(backupDir string, day time.Time) []string {
	d := day.Format("2006-01-02")
	var out []string
	for _, p := range []string{
		filepath.Join(backupDir, d),
		filepath.Join(backupDir, "weekly", d),
		filepath.Join(backupDir, "monthly", d),
	} {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

type Archive struct {
	User  string
	Path  string
	Kind  string // tar.gz | tar | dir
	Bytes int64
}

// Scan lists <dir>/accounts entries, keyed by user. When a user appears in
// several dirs (daily + weekly on the same day) the first dir wins.
func Scan(dirs []string) (map[string]Archive, error) {
	out := map[string]Archive{}
	for _, d := range dirs {
		ents, err := os.ReadDir(filepath.Join(d, "accounts"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return out, err
		}
		for _, e := range ents {
			name := e.Name()
			p := filepath.Join(d, "accounts", name)
			var a Archive
			switch {
			case e.IsDir():
				a = Archive{User: name, Path: p, Kind: "dir"}
				a.Bytes = dirSize(p)
			case strings.HasSuffix(name, ".tar.gz"):
				a = Archive{User: strings.TrimSuffix(name, ".tar.gz"), Path: p, Kind: "tar.gz"}
			case strings.HasSuffix(name, ".tar"):
				a = Archive{User: strings.TrimSuffix(name, ".tar"), Path: p, Kind: "tar"}
			default:
				continue
			}
			if a.Kind != "dir" {
				if fi, err := e.Info(); err == nil {
					a.Bytes = fi.Size()
				}
			}
			if _, seen := out[a.User]; !seen {
				out[a.User] = a
			}
		}
	}
	return out, nil
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, e := d.Info(); e == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

type VerifyResult struct {
	Sampled    int      `json:"sampled"`
	OK         int      `json:"ok"`
	Failed     []string `json:"failed,omitempty"`
	Skipped    int      `json:"skipped,omitempty"` // dir-type (incremental) archives
	Incomplete bool     `json:"incomplete,omitempty"`
	Seconds    float64  `json:"seconds"`
}

// Verify reads a random sample of archives end to end (gzip + tar framing),
// proving they open. n is capped by maxPct of the archives (at least 1) and
// the whole pass by ctx. Running out of time is "incomplete", not a failure.
func Verify(ctx context.Context, archives map[string]Archive, n int, maxPct float64, rng *rand.Rand) VerifyResult {
	start := time.Now()
	var res VerifyResult
	var pool []Archive
	for _, a := range archives {
		if a.Kind == "dir" {
			res.Skipped++
			continue
		}
		pool = append(pool, a)
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].User < pool[j].User })
	if cap := int(float64(len(pool)) * maxPct / 100); n > cap {
		n = cap
		if n < 1 && len(pool) > 0 {
			n = 1
		}
	}
	if n > len(pool) {
		n = len(pool)
	}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	for _, a := range pool[:n] {
		if ctx.Err() != nil {
			res.Incomplete = true
			break
		}
		err := readArchive(ctx, a)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			res.Incomplete = true
			break
		}
		res.Sampled++
		if err != nil {
			res.Failed = append(res.Failed, a.User)
		} else {
			res.OK++
		}
	}
	res.Seconds = float64(time.Since(start).Milliseconds()) / 1000
	return res
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func readArchive(ctx context.Context, a Archive) error {
	f, err := os.Open(a.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = ctxReader{ctx, f}
	if a.Kind == "tar.gz" {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	entries := 0
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
		entries++
	}
	if entries == 0 {
		return errors.New("empty archive")
	}
	return nil
}
