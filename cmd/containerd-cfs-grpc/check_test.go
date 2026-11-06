// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	introspectionapi "github.com/containerd/containerd/api/services/introspection/v1"
	versionapi "github.com/containerd/containerd/api/services/version/v1"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/foundriesio/cfs-snapshotter/internal/snapshot"
)

// storeVerity is what the check must say about the ocicfs store at a root:
// its fs-verity verdict line, and whether that line fails an otherwise
// passing check, which a required build's does on a filesystem that cannot
// enable fs-verity. Which one a t.TempDir is depends on the host and the
// build mode, so expectStoreVerity finds out with a throwaway engine on the
// same store.
type storeVerity struct {
	line  string
	fails bool
}

func expectStoreVerity(t *testing.T, root string) storeVerity {
	t.Helper()
	engine, err := ocicfs.New(ocicfs.Config{Root: root})
	if err != nil {
		t.Fatalf("ocicfs.New() error = %v", err)
	}
	state, err := engine.VerityState()
	if err != nil {
		t.Fatalf("VerityState() error = %v", err)
	}
	prefix := fmt.Sprintf("2. fs-verity on the ocicfs store (%s): ", root)
	switch state {
	case "unsupported":
		return storeVerity{line: prefix + "FAIL: unsupported (build: " + ocicfs.BuildVerityMode + ")", fails: true}
	case "disabled":
		return storeVerity{line: prefix + "disabled (build: " + ocicfs.BuildVerityMode + "): mounts do not verify object contents"}
	}
	return storeVerity{line: prefix + "enabled (build: " + ocicfs.BuildVerityMode + ")"}
}

// verdict returns the exit code and the last line of a check that fails on
// nothing but, possibly, the store.
func (s storeVerity) verdict() (int, string) {
	if s.fails {
		return 1, "check failed"
	}
	return 0, "check passed"
}

// fakeContainerd serves the version and introspection fakes on a fresh unix
// socket and returns its address.
func fakeContainerd(t *testing.T, version string, plugins []*introspectionapi.Plugin) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "containerd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer(grpc.Creds(insecure.NewCredentials()))
	versionapi.RegisterVersionServer(server, &fakeVersionServer{version: version})
	introspectionapi.RegisterIntrospectionServer(server, &fakeIntrospectionServer{plugins: plugins})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return socket
}

// wantLines asserts the report contains every want, in order of appearance
// in the wants list.
func wantLines(t *testing.T, report string, wants ...string) {
	t.Helper()
	rest := report
	for _, want := range wants {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("report missing %q (in order):\n%s", want, report)
		}
		rest = rest[i+len(want):]
	}
}

func TestRunCheckPassesOutOfBand(t *testing.T) {
	address := fakeContainerd(t, "v2.2.3", wiredPlugins(map[string]string{"enable_remote_snapshot_annotations": "true"}))
	root := t.TempDir()
	store := expectStoreVerity(t, root)
	wantCode, verdict := store.verdict()
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: root, ContainerdAddress: address}, testSocket)
	if code != wantCode {
		t.Fatalf("runCheck = %d, want %d; output:\n%s", code, wantCode, out.String())
	}
	wantLines(t, out.String(),
		"1. mkcomposefs: skipped (preloaded mode",
		store.line,
		"ingest workers: 2",
		"3. containerd: ok (version v2.2.3)",
		"4. containerd version 2.1+: ok",
		"5. containerd proxy_plugins wiring: ok (proxy plugin cfs)",
		"6. enable_remote_snapshot_annotations export: ok",
		"7. containerd diff proxy_plugins wiring: ok (proxy plugin cfs-diff)",
		verdict)
}

func TestRunCheckReportsConfiguredWorkers(t *testing.T) {
	address := fakeContainerd(t, "v2.2.3", wiredPlugins(map[string]string{"enable_remote_snapshot_annotations": "true"}))
	root := t.TempDir()
	store := expectStoreVerity(t, root)
	wantCode, verdict := store.verdict()
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: root, ContainerdAddress: address, IngestWorkers: 4}, testSocket)
	if code != wantCode {
		t.Fatalf("runCheck = %d, want %d; output:\n%s", code, wantCode, out.String())
	}
	wantLines(t, out.String(), "ingest workers: 4", verdict)
}

func TestRunCheckFailsOnOldContainerd(t *testing.T) {
	address := fakeContainerd(t, "1.7.24~ds1", wiredPlugins(map[string]string{"enable_remote_snapshot_annotations": "true"}))
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: t.TempDir(), ContainerdAddress: address}, testSocket)
	if code != 1 {
		t.Fatalf("runCheck = %d, want 1; output:\n%s", code, out.String())
	}
	// The version failure must not stop the remaining checks from being
	// reported: the wiring is inspected and shown as fine.
	wantLines(t, out.String(),
		"4. containerd version 2.1+: FAIL: 1.7.24~ds1 is older than 2.1",
		"5. containerd proxy_plugins wiring: ok (proxy plugin cfs)",
		"6. enable_remote_snapshot_annotations export: ok",
		"7. containerd diff proxy_plugins wiring: ok (proxy plugin cfs-diff)",
		"check failed")
}

func TestRunCheckFailsOnMissingDiffer(t *testing.T) {
	address := fakeContainerd(t, "v2.2.3",
		[]*introspectionapi.Plugin{wiredPlugin(map[string]string{"enable_remote_snapshot_annotations": "true"})})
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: t.TempDir(), ContainerdAddress: address, AdmitAny: true},
		testSocket)
	if code != 1 {
		t.Fatalf("runCheck = %d, want 1; output:\n%s", code, out.String())
	}
	wantLines(t, out.String(),
		"5. containerd proxy_plugins wiring: ok (proxy plugin cfs)",
		"6. enable_remote_snapshot_annotations export: ok",
		"7. containerd diff proxy_plugins wiring: FAIL: no proxy_plugins entry of type \"diff\" points at",
		"check failed")
}

func TestRunCheckFailsOnUnreachableContainerd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	code := runCheck(ctx, &out,
		snapshot.Config{OcicfsRoot: t.TempDir(), ContainerdAddress: filepath.Join(t.TempDir(), "absent.sock")},
		testSocket)
	if code != 1 {
		t.Fatalf("runCheck = %d, want 1; output:\n%s", code, out.String())
	}
	wantLines(t, out.String(),
		"3. containerd: FAIL: no answer at",
		"4. containerd version 2.1+: not checked (containerd did not answer)",
		"5. containerd proxy_plugins wiring: not checked (containerd did not answer)",
		"6. enable_remote_snapshot_annotations export: not checked (containerd did not answer)",
		"7. containerd diff proxy_plugins wiring: not checked (containerd did not answer)",
		"check failed")
}

func TestRunCheckFailsOnMissingAnnotationsExport(t *testing.T) {
	address := fakeContainerd(t, "v2.2.3", wiredPlugins(nil))
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: t.TempDir(), ContainerdAddress: address}, testSocket)
	if code != 1 {
		t.Fatalf("runCheck = %d, want 1; output:\n%s", code, out.String())
	}
	wantLines(t, out.String(),
		"5. containerd proxy_plugins wiring: ok (proxy plugin cfs)",
		"6. enable_remote_snapshot_annotations export: FAIL: missing enable_remote_snapshot_annotations",
		"7. containerd diff proxy_plugins wiring: ok (proxy plugin cfs-diff)",
		"check failed")
}

func TestRunCheckReportsAllStagedPrerequisites(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	address := fakeContainerd(t, "v2.2.3", wiredPlugins(map[string]string{"enable_remote_snapshot_annotations": "true"}))
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: t.TempDir(), ContainerdAddress: address, AdmitAny: true},
		testSocket)
	if code != 1 {
		t.Fatalf("runCheck = %d, want 1; output:\n%s", code, out.String())
	}
	// The failed mkcomposefs lookup must not hide the fs-verity verdict:
	// both prerequisites appear, each with its own result.
	wantLines(t, out.String(),
		"1. mkcomposefs: FAIL:",
		"2. fs-verity on the ocicfs store (",
		"3. containerd: ok (version v2.2.3)",
		"7. containerd diff proxy_plugins wiring: ok (proxy plugin cfs-diff)",
		"check failed")
	if !strings.Contains(out.String(), "mkcomposefs not found") {
		t.Fatalf("mkcomposefs failure lacks its cause:\n%s", out.String())
	}
}

// TestRunCheckWarnsOnMissingDifferOutOfBand covers that preloaded mode
// applies loaded layers only from the store, so missing differ wiring that
// fails the check in admit-any mode is only a warning here.
func TestRunCheckWarnsOnMissingDifferOutOfBand(t *testing.T) {
	address := fakeContainerd(t, "v2.2.3",
		[]*introspectionapi.Plugin{wiredPlugin(map[string]string{"enable_remote_snapshot_annotations": "true"})})
	root := t.TempDir()
	store := expectStoreVerity(t, root)
	wantCode, verdict := store.verdict()
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: root, ContainerdAddress: address}, testSocket)
	if code != wantCode {
		t.Fatalf("runCheck = %d, want %d; output:\n%s", code, wantCode, out.String())
	}
	wantLines(t, out.String(),
		"6. enable_remote_snapshot_annotations export: ok",
		"7. containerd diff proxy_plugins wiring: WARN: no proxy_plugins entry of type \"diff\" points at",
		verdict)
}
