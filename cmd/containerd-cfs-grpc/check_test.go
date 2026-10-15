// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	introspectionapi "github.com/containerd/containerd/api/services/introspection/v1"
	versionapi "github.com/containerd/containerd/api/services/version/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/foundriesio/cfs-snapshotter/internal/snapshot"
)

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
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: t.TempDir(), ContainerdAddress: address}, testSocket)
	if code != 0 {
		t.Fatalf("runCheck = %d, want 0; output:\n%s", code, out.String())
	}
	wantLines(t, out.String(),
		"1. mkcomposefs: skipped (out-of-band mode",
		"2. fs-verity on the ocicfs store: skipped (out-of-band mode",
		"ingest workers: 2",
		"3. containerd: ok (version v2.2.3)",
		"4. containerd version 2.1+: ok",
		"5. containerd proxy_plugins wiring: ok (proxy plugin cfs)",
		"6. enable_remote_snapshot_annotations export: ok",
		"7. containerd diff proxy_plugins wiring: skipped (out-of-band mode applies no layers)",
		"check passed")
}

func TestRunCheckReportsConfiguredWorkers(t *testing.T) {
	address := fakeContainerd(t, "v2.2.3", wiredPlugins(map[string]string{"enable_remote_snapshot_annotations": "true"}))
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: t.TempDir(), ContainerdAddress: address, IngestWorkers: 4}, testSocket)
	if code != 0 {
		t.Fatalf("runCheck = %d, want 0; output:\n%s", code, out.String())
	}
	wantLines(t, out.String(), "ingest workers: 4", "check passed")
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
		"7. containerd diff proxy_plugins wiring: skipped (out-of-band mode applies no layers)",
		"check failed")
}

func TestRunCheckFailsOnMissingDiffer(t *testing.T) {
	address := fakeContainerd(t, "v2.2.3",
		[]*introspectionapi.Plugin{wiredPlugin(map[string]string{"enable_remote_snapshot_annotations": "true"})})
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: t.TempDir(), ContainerdAddress: address, AllowUnstagedPulls: true},
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
		"7. containerd diff proxy_plugins wiring: skipped (out-of-band mode applies no layers)",
		"check failed")
}

func TestRunCheckReportsAllStagedPrerequisites(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	address := fakeContainerd(t, "v2.2.3", wiredPlugins(map[string]string{"enable_remote_snapshot_annotations": "true"}))
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: t.TempDir(), ContainerdAddress: address, AllowUnstagedPulls: true},
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

// TestRunCheckSkipsDifferOutOfBand covers that the mode which applies no
// layers does not require the differ the staged mode needs: the same wiring
// that fails the check in staged mode passes here.
func TestRunCheckSkipsDifferOutOfBand(t *testing.T) {
	address := fakeContainerd(t, "v2.2.3",
		[]*introspectionapi.Plugin{wiredPlugin(map[string]string{"enable_remote_snapshot_annotations": "true"})})
	var out bytes.Buffer
	code := runCheck(context.Background(), &out,
		snapshot.Config{OcicfsRoot: t.TempDir(), ContainerdAddress: address}, testSocket)
	if code != 0 {
		t.Fatalf("runCheck = %d, want 0; output:\n%s", code, out.String())
	}
	wantLines(t, out.String(),
		"6. enable_remote_snapshot_annotations export: ok",
		"7. containerd diff proxy_plugins wiring: skipped (out-of-band mode applies no layers)",
		"check passed")
}
