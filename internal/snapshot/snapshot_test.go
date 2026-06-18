// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
)

func TestNewSnapshotterRejectsEmptyRoot(t *testing.T) {
	if _, err := NewSnapshotter(context.Background(), ""); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("NewSnapshotter(%q) error = %v, want invalid argument", "", err)
	}
}

func TestNewSnapshotterAcceptsValidRoot(t *testing.T) {
	sn, err := NewSnapshotter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}
	if sn == nil {
		t.Fatal("NewSnapshotter() returned a nil snapshotter")
	}
}

// TestStubMethodsReturnNotImplemented locks the current contract: every
// snapshots.Snapshotter method is a stub returning errdefs.ErrNotImplemented.
// Implementing the backend should change these deliberately.
func TestStubMethodsReturnNotImplemented(t *testing.T) {
	ctx := context.Background()
	sn, err := NewSnapshotter(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}

	tests := []struct {
		name string
		call func() error
	}{
		{"Stat", func() error { _, err := sn.Stat(ctx, "key"); return err }},
		{"Update", func() error { _, err := sn.Update(ctx, snapshots.Info{}); return err }},
		{"Usage", func() error { _, err := sn.Usage(ctx, "key"); return err }},
		{"Mounts", func() error { _, err := sn.Mounts(ctx, "key"); return err }},
		{"Prepare", func() error { _, err := sn.Prepare(ctx, "key", ""); return err }},
		{"View", func() error { _, err := sn.View(ctx, "key", ""); return err }},
		{"Commit", func() error { return sn.Commit(ctx, "name", "key") }},
		{"Remove", func() error { return sn.Remove(ctx, "key") }},
		{"Walk", func() error {
			return sn.Walk(ctx, func(context.Context, snapshots.Info) error { return nil })
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errdefs.IsNotImplemented(err) {
				t.Fatalf("%s() error = %v, want not implemented", tt.name, err)
			}
		})
	}
}

func TestPrepareAppliesOptions(t *testing.T) {
	ctx := context.Background()
	sn, err := NewSnapshotter(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}

	// A successful option is applied before the stub returns not implemented.
	applied := false
	ok := func(*snapshots.Info) error {
		applied = true
		return nil
	}
	if _, err := sn.Prepare(ctx, "key", "", ok); !errdefs.IsNotImplemented(err) {
		t.Fatalf("Prepare() error = %v, want not implemented", err)
	}
	if !applied {
		t.Fatal("Prepare() did not apply the snapshot option")
	}

	// A failing option surfaces its error instead of not implemented.
	optErr := errors.New("boom")
	failing := func(*snapshots.Info) error { return optErr }
	_, err = sn.Prepare(ctx, "key", "", failing)
	if !errors.Is(err, optErr) {
		t.Fatalf("Prepare() with failing option error = %v, want wrapped %v", err, optErr)
	}
	if errdefs.IsNotImplemented(err) {
		t.Fatal("Prepare() with failing option returned not implemented, want the option error")
	}
}
