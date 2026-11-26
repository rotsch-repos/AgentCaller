//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// One account, one process: while a call holds the store, a second call, a message or a
// pairing on the same store is turned away at once instead of taking over the
// connection. When the first is done, the store is free again.
func TestStoreIsHeldByOneProcess(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.db")
	ctx := context.Background()
	first, err := openSession(ctx, path, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openSession(ctx, path, zerolog.Nop()); !errors.Is(err, errStoreBusy) {
		t.Fatalf("second session on a held store: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := openSession(ctx, path, zerolog.Nop())
	if err != nil {
		t.Fatalf("the store was not released: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
