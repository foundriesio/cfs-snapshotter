// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	introspectionapi "github.com/containerd/containerd/api/services/introspection/v1"
	versionapi "github.com/containerd/containerd/api/services/version/v1"
	"github.com/containerd/log"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/foundriesio/cfs-snapshotter/internal/snapshot"
)

func TestParseMajorMinor(t *testing.T) {
	for _, tc := range []struct {
		version      string
		major, minor int
		ok           bool
	}{
		{"v2.2.3", 2, 2, true},
		{"2.1.0", 2, 1, true},
		{"1.7.24~ds1", 1, 7, true},
		{"v2.0.0-rc.4", 2, 0, true},
		{"v2.4-beta", 2, 4, true},
		{"weird", 0, 0, false},
		{"", 0, 0, false},
		{"x.y.z", 0, 0, false},
	} {
		major, minor, ok := parseMajorMinor(tc.version)
		if major != tc.major || minor != tc.minor || ok != tc.ok {
			t.Errorf("parseMajorMinor(%q) = (%d, %d, %v), want (%d, %d, %v)",
				tc.version, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}

type fakeVersionServer struct {
	versionapi.UnimplementedVersionServer
	version string
}

func (f *fakeVersionServer) Version(context.Context, *emptypb.Empty) (*versionapi.VersionResponse, error) {
	return &versionapi.VersionResponse{Version: f.version}, nil
}

type fakeIntrospectionServer struct {
	introspectionapi.UnimplementedIntrospectionServer
	mu      sync.Mutex
	plugins []*introspectionapi.Plugin
}

func (f *fakeIntrospectionServer) Plugins(context.Context, *introspectionapi.PluginsRequest) (*introspectionapi.PluginsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &introspectionapi.PluginsResponse{Plugins: f.plugins}, nil
}

func (f *fakeIntrospectionServer) setPlugins(plugins []*introspectionapi.Plugin) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plugins = plugins
}

// runProbe serves a fake containerd with the given version and plugins on a
// unix socket, runs the probe against it as the snapshotter at socketAddress,
// and returns the log entries the probe emitted, the STATUS line it pushed to
// a fake systemd notification socket ("" when none arrived), and the probe's
// return value — non-nil exactly when the first verdict was binding.
func runProbe(t *testing.T, version string, plugins []*introspectionapi.Plugin, socketAddress string) ([]*logrus.Entry, string, error) {
	t.Helper()
	containerdSocket := filepath.Join(t.TempDir(), "containerd.sock")
	listener, err := net.Listen("unix", containerdSocket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	versionapi.RegisterVersionServer(server, &fakeVersionServer{version: version})
	introspectionapi.RegisterIntrospectionServer(server, &fakeIntrospectionServer{plugins: plugins})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	notifySocket := filepath.Join(t.TempDir(), "notify.sock")
	notifyConn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifySocket, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen notify socket: %v", err)
	}
	t.Cleanup(func() { _ = notifyConn.Close() })
	t.Setenv("NOTIFY_SOCKET", notifySocket)

	// The probe loops until its context ends; the first round's STATUS
	// datagram is the signal that the round, logging included, is complete.
	logger, hook := logrustest.NewNullLogger()
	ctx, cancel := context.WithCancel(log.WithLogger(context.Background(), logrus.NewEntry(logger)))
	done := make(chan error, 1)
	go func() {
		done <- probeContainerd(ctx, containerdSocket, socketAddress)
	}()

	status := ""
	if err := notifyConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set notify read deadline: %v", err)
	}
	buf := make([]byte, 512)
	if n, _, err := notifyConn.ReadFromUnix(buf); err == nil {
		status = string(buf[:n])
	}
	cancel()
	return hook.AllEntries(), status, <-done
}

// entryContaining asserts that some logged message contains want at the given
// level.
func entryContaining(t *testing.T, entries []*logrus.Entry, level logrus.Level, want string) {
	t.Helper()
	for _, e := range entries {
		if e.Level == level && strings.Contains(e.Message, want) {
			return
		}
	}
	t.Fatalf("no %s log entry containing %q in %v", level, want, entries)
}

const testSocket = "/run/containerd-cfs-grpc/containerd-cfs-grpc.sock"

func wiredPlugin(exports map[string]string) *introspectionapi.Plugin {
	if exports == nil {
		exports = map[string]string{}
	}
	exports["address"] = testSocket
	return &introspectionapi.Plugin{Type: snapshotterPluginType, ID: "cfs", Exports: exports}
}

func TestProbeReportsOldContainerd(t *testing.T) {
	entries, status, err := runProbe(t, "1.7.24~ds1",
		[]*introspectionapi.Plugin{wiredPlugin(map[string]string{"enable_remote_snapshot_annotations": "true"})},
		testSocket)
	entryContaining(t, entries, logrus.ErrorLevel, "older than 2.1")
	// Correct wiring must not be announced as success while the version
	// already makes every pull fail.
	for _, e := range entries {
		if strings.Contains(e.Message, "wired") {
			t.Fatalf("unexpected wired entry alongside the version error: %v", e.Message)
		}
	}
	if want := "STATUS=degraded: containerd 1.7.24~ds1 is older than 2.1; pulls will fail"; status != want {
		t.Fatalf("status = %q, want %q", status, want)
	}
	if !errors.Is(err, snapshot.ErrPrerequisites) {
		t.Fatalf("probe returned %v, want a binding prerequisites failure", err)
	}
}

func TestProbeReportsMissingProxyEntry(t *testing.T) {
	entries, status, err := runProbe(t, "v2.2.3", nil, testSocket)
	entryContaining(t, entries, logrus.ErrorLevel, "no proxy snapshotter")
	if !strings.Contains(status, "degraded: containerd has no proxy_plugins entry") {
		t.Fatalf("status = %q, want the missing-entry degradation", status)
	}
	if !errors.Is(err, snapshot.ErrPrerequisites) {
		t.Fatalf("probe returned %v, want a binding prerequisites failure", err)
	}
}

func TestProbeReportsMissingAnnotationsExport(t *testing.T) {
	entries, status, err := runProbe(t, "v2.2.3",
		[]*introspectionapi.Plugin{wiredPlugin(nil)},
		testSocket)
	entryContaining(t, entries, logrus.ErrorLevel, "enable_remote_snapshot_annotations")
	if !strings.Contains(status, "degraded: proxy_plugins.cfs lacks enable_remote_snapshot_annotations") {
		t.Fatalf("status = %q, want the missing-export degradation", status)
	}
	if !errors.Is(err, snapshot.ErrPrerequisites) {
		t.Fatalf("probe returned %v, want a binding prerequisites failure", err)
	}
}

// TestProbeReportsRecovery covers that after a healthy first verdict the
// rounds only inform: the wiring disappears (the probe reports drift and does
// not return), then reappears, and the status flips back to ok without a
// daemon restart.
func TestProbeReportsRecovery(t *testing.T) {
	old := reprobeInterval
	reprobeInterval = 20 * time.Millisecond
	t.Cleanup(func() { reprobeInterval = old })

	containerdSocket := filepath.Join(t.TempDir(), "containerd.sock")
	listener, err := net.Listen("unix", containerdSocket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	wired := []*introspectionapi.Plugin{wiredPlugin(map[string]string{"enable_remote_snapshot_annotations": "true"})}
	introspection := &fakeIntrospectionServer{plugins: wired}
	server := grpc.NewServer()
	versionapi.RegisterVersionServer(server, &fakeVersionServer{version: "v2.2.3"})
	introspectionapi.RegisterIntrospectionServer(server, introspection)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	notifySocket := filepath.Join(t.TempDir(), "notify.sock")
	notifyConn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifySocket, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen notify socket: %v", err)
	}
	t.Cleanup(func() { _ = notifyConn.Close() })
	t.Setenv("NOTIFY_SOCKET", notifySocket)

	logger, _ := logrustest.NewNullLogger()
	ctx, cancel := context.WithCancel(log.WithLogger(context.Background(), logrus.NewEntry(logger)))
	done := make(chan error, 1)
	go func() {
		done <- probeContainerd(ctx, containerdSocket, testSocket)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("probe returned %v, want nil: drift after a healthy first verdict must not be binding", err)
		}
	})

	readStatus := func() string {
		t.Helper()
		if err := notifyConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set notify read deadline: %v", err)
		}
		buf := make([]byte, 512)
		n, _, err := notifyConn.ReadFromUnix(buf)
		if err != nil {
			t.Fatalf("read notify datagram: %v", err)
		}
		return string(buf[:n])
	}

	if status := readStatus(); status != "STATUS=ok: wired to containerd v2.2.3 (proxy plugin cfs)" {
		t.Fatalf("first status = %q, want ok", status)
	}

	introspection.setPlugins(nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := readStatus()
		if strings.Contains(status, "degraded: containerd has no proxy_plugins entry") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never degraded, last = %q", status)
		}
	}

	introspection.setPlugins(wired)
	deadline = time.Now().Add(2 * time.Second)
	for {
		status := readStatus()
		if status == "STATUS=ok: wired to containerd v2.2.3 (proxy plugin cfs)" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never recovered, last = %q", status)
		}
	}
}

func TestProbeReportsWiredContainerd(t *testing.T) {
	entries, status, err := runProbe(t, "v2.2.3",
		[]*introspectionapi.Plugin{wiredPlugin(map[string]string{"enable_remote_snapshot_annotations": "true"})},
		testSocket)
	for _, e := range entries {
		if e.Level <= logrus.ErrorLevel {
			t.Fatalf("unexpected error entry: %v", e.Message)
		}
	}
	entryContaining(t, entries, logrus.InfoLevel, "wired")
	if want := "STATUS=ok: wired to containerd v2.2.3 (proxy plugin cfs)"; status != want {
		t.Fatalf("status = %q, want %q", status, want)
	}
	if err != nil {
		t.Fatalf("probe returned %v, want nil for a healthy first verdict", err)
	}
}
