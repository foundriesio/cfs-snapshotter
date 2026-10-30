// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/opencontainers/go-digest"
)

// loadKey is the key containerd's client-side unpacker files the record of
// the layer chain under, as it reaches the snapshotter.
func loadKey(n int, chain digest.Digest) string {
	return fmt.Sprintf("moby/%d/extract-%d-abc %s", n, n, chain)
}

func TestUnpackChainID(t *testing.T) {
	chain := digest.FromString("chain")
	tests := []struct {
		key  string
		want string
	}{
		{key: "moby/3/extract-1-abc " + chain.String(), want: chain.String()},
		{key: "extract-1-abc " + chain.String(), want: chain.String()},
		{key: "moby/3/extract-1-abc" + chain.String()},
		{key: "moby/3/extract-1-abc sha512:" + digest.SHA512.FromString("chain").Encoded()},
		{key: "moby/3/extract-1-abc sha256:nothex"},
		{key: "moby/3/0123456789abcdef-init"},
		{key: "moby/extract-1-abc " + chain.String() + "/3/c"},
		{key: "moby/3/prepare-1-abc " + chain.String()},
		{key: ""},
	}
	for _, tt := range tests {
		got, ok := unpackChainID(tt.key)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("unpackChainID(%q) = %q, %v; want %q", tt.key, got, ok, tt.want)
		}
	}
}

// TestPrepareLoadHandshakeStages covers the handshake of a client-side
// unpack, which labels nothing: in either mode it gets a staging record
// labeled with the chain its key names and the placeholder mount, owns no
// directory, and a second unpack of the same chain under another key is
// staged too rather than answered as already present.
func TestPrepareLoadHandshakeStages(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprintf("staged=%v", staged), func(t *testing.T) {
			ctx := pullNamespace()
			sn := newConfiguredSnapshotter(t, Config{Root: t.TempDir(), AllowUnstagedPulls: staged}, fakeEngineForImages(), notMounted)
			chain := digest.FromString("chain")
			for n := range 2 {
				key := loadKey(n, chain)
				mounts, err := sn.Prepare(ctx, key, "")
				if err != nil {
					t.Fatalf("Prepare(%s) error = %v", key, err)
				}
				if !isStagingMount(mounts, key) {
					t.Fatalf("mounts = %v, want the staging placeholder naming the record", mounts)
				}
				id, info, err := sn.snapshotInfo(ctx, key)
				if err != nil {
					t.Fatalf("snapshotInfo(%s) error = %v", key, err)
				}
				if info.Labels[stagingLabel] == "" || info.Labels[intermediateSnapshotLabel] != chain.String() {
					t.Fatalf("record labels = %v, want a staging record standing for %s", info.Labels, chain)
				}
				if _, err := os.Stat(sn.getSnapshotDir(id)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("snapshot directory error = %v, want none provisioned", err)
				}
			}
		})
	}
}

// TestPrepareLoadHandshakeNeverPromotes covers an unpack handshake over a
// committed staged record that is no chain member: it is staged like any
// other, with no promotion started from inside the unpack.
func TestPrepareLoadHandshakeNeverPromotes(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	d := pullDerived(t, sn)
	builds, lists := len(engine.builds), engine.lists

	key := loadKey(7, digest.FromString("on-top"))
	mounts, err := sn.Prepare(ctx, key, d.base)
	if err != nil {
		t.Fatalf("Prepare(%s) error = %v", key, err)
	}
	if !isStagingMount(mounts, key) {
		t.Fatalf("mounts = %v, want the staging placeholder", mounts)
	}
	if len(engine.builds) != builds || engine.lists != lists || imagesOf(sn).listCount() != 0 {
		t.Fatalf("builds = %d, store lists = %d, image listings = %d; want nothing started", len(engine.builds)-builds, engine.lists-lists, imagesOf(sn).listCount())
	}
	assertUnpromoted(t, sn, d)
}
