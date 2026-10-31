package spool

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/servatum-io/servatum-cli/internal/api"
)

func TestExpiryAndCaps(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	Save(dir, "k1", api.Run{Job: "j", Status: "ok", IdempotencyKey: "old"}, now.Add(-73*time.Hour))
	Save(dir, "k1", api.Run{Job: "j", Status: "ok", IdempotencyKey: "new"}, now)
	var sent []string
	res, err := Flush(dir, "k1", now, time.Time{}, func(r api.Run) error { sent = append(sent, r.IdempotencyKey); return nil })
	if err != nil || res.Expired != 1 || res.Sent != 1 || len(sent) != 1 || sent[0] != "new" {
		t.Fatalf("res=%+v sent=%v err=%v", res, sent, err)
	}
	for i := 0; i < MaxFiles+5; i++ {
		Save(dir, "k1", api.Run{Job: "j", Status: "ok"}, now.Add(time.Duration(i)))
	}
	es, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(es) != MaxFiles {
		t.Fatalf("cap: %d files", len(es))
	}
}

func TestStopsOnTransientKeepsRest(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := 0; i < 3; i++ {
		Save(dir, "k1", api.Run{Job: "j", Status: "ok"}, now.Add(time.Duration(i)))
	}
	res, err := Flush(dir, "k1", now, time.Time{}, func(api.Run) error { return errors.New("network down") })
	es, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if err == nil || res.Left != 3 || len(es) != 3 {
		t.Fatalf("res=%+v files=%d err=%v", res, len(es), err)
	}
	info, _ := os.Stat(es[0])
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}
