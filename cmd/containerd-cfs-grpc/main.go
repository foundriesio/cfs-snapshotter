// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Command containerd-cfs-grpc is the cfs-snapshotter daemon: a containerd
// remote (proxy) snapshotter and the differ that ingests its staged pulls,
// both exposed over one gRPC unix socket. containerd is pointed at them via
// the [proxy_plugins.cfs] and [proxy_plugins.cfs-diff] entries in its
// config.toml (see docs/running.md). Image content is served from composefs
// images in the local knarr store.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	diffapi "github.com/containerd/containerd/api/services/diff/v1"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/log"
	"github.com/foundriesio/knarr/pkg/knarr"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"

	"github.com/foundriesio/cfs-snapshotter/internal/snapshot"
	"github.com/foundriesio/cfs-snapshotter/pkg/version"
)

const (
	defaultAddress = "/run/containerd-cfs-grpc/containerd-cfs-grpc.sock"
	defaultRootDir = "/var/lib/containerd-cfs-grpc"
)

var (
	address            = flag.String("address", defaultAddress, "address for the snapshotter's gRPC server")
	rootDir            = flag.String("root", defaultRootDir, "path to the root directory for this snapshotter")
	knarrRoot          = flag.String("knarr-root", "/var/lib/knarr", "path to the shared knarr image store root")
	containerdAddress  = flag.String("containerd-address", "/run/containerd/containerd.sock", "address of containerd's gRPC server")
	allowUnstagedPulls = flag.Bool("allow-unstaged-pulls", true, "stage every pull through containerd's content store and build it into the knarr store, instead of admitting only images already there")
	ingestWorkers      = flag.Int("ingest-workers", 0, fmt.Sprintf("number of layers a staged pull ingests into the knarr store at once, across all pulls in progress (0 selects the default of %d)", knarr.DefaultIngestWorkers))
	logLevel           = flag.String("log-level", log.InfoLevel.String(), "logging level [trace, debug, info, warn, error, fatal, panic]")
	printVersion       = flag.Bool("version", false, "print the version and exit")
	runChecks          = flag.Bool("check", false, "check the runtime prerequisites and the containerd wiring instead of serving, and exit non-zero on any failure")
)

func main() {
	flag.Parse()

	if *printVersion {
		fmt.Println("containerd-cfs-grpc", version.String())
		return
	}

	if err := log.SetFormat(log.JSONFormat); err != nil {
		fmt.Fprintln(os.Stderr, "set log format:", err)
		os.Exit(1)
	}
	if err := log.SetLevel(*logLevel); err != nil {
		fmt.Fprintln(os.Stderr, "invalid log level:", err)
		os.Exit(1)
	}

	ctx := log.WithLogger(context.Background(), log.L)
	cfg := snapshot.Config{
		Root:               *rootDir,
		KnarrRoot:          *knarrRoot,
		AllowUnstagedPulls: *allowUnstagedPulls,
		ContainerdAddress:  *containerdAddress,
		IngestWorkers:      *ingestWorkers,
	}
	if *runChecks {
		// Bounded: a check answers now, it does not wait for containerd the
		// way the serving daemon's probe does. The window still spans a few
		// of the probe's retry beats for a containerd that is just starting.
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		os.Exit(runCheck(ctx, os.Stdout, cfg, *address))
	}
	if err := run(ctx, *address, cfg); err != nil {
		if errors.Is(err, snapshot.ErrPrerequisites) {
			log.G(ctx).WithError(err).
				Error("cfs-snapshotter exiting: install or configure the missing prerequisite, then systemctl restart cfs-snapshotter")
			os.Exit(exitPrerequisites)
		}
		log.G(ctx).WithError(err).Fatal("cfs-snapshotter exited with error")
	}
}

// exitPrerequisites is the exit status for unmet runtime prerequisites. The
// packaged unit lists it in RestartPreventExitStatus, so the service lands in
// the failed state at once instead of cycling through automatic restarts that
// cannot succeed until an administrator fixes the host or containerd.
const exitPrerequisites = 3

// run builds the snapshotter, serves it and its differ over a gRPC unix
// socket, and blocks until SIGINT/SIGTERM, then shuts down gracefully.
func run(ctx context.Context, address string, cfg snapshot.Config) error {
	sn, err := snapshot.NewSnapshotter(ctx, cfg)
	if err != nil {
		return fmt.Errorf("create snapshotter: %w", err)
	}
	defer func() {
		if err := sn.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("close snapshotter")
		}
	}()

	listener, err := listen(address)
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(address); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.G(ctx).WithError(err).WithField("address", address).Warn("remove socket")
		}
	}()

	server := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(server, snapshotservice.FromSnapshotter(sn))
	diffapi.RegisterDiffServer(server, newDiffServer(sn))

	errCh := make(chan error, 1)
	go func() {
		log.G(ctx).WithField("address", address).Info("serving cfs-snapshotter")
		if serveErr := server.Serve(listener); serveErr != nil {
			errCh <- fmt.Errorf("serve %q: %w", address, serveErr)
		}
	}()

	// The probe's first verdict is binding (see probeContainerd): a
	// containerd that cannot drive this snapshotter ends the daemon through
	// the same error path as any other startup failure.
	probeErrCh := make(chan error, 1)
	go func() {
		if probeErr := probeContainerd(ctx, cfg.ContainerdAddress, address, cfg.AllowUnstagedPulls); probeErr != nil {
			probeErrCh <- probeErr
		}
	}()
	notify(ctx, "READY=1\nSTATUS=serving")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM)
	select {
	case err := <-probeErrCh:
		return err
	case sig := <-sigCh:
		log.G(ctx).WithField("signal", sig).Info("shutting down")
		notify(ctx, "STOPPING=1")
		// GracefulStop waits for in-flight RPCs indefinitely; force a hard
		// stop if it does not finish in time or a second signal arrives, so a
		// stuck mount or store operation cannot wedge the shutdown.
		done := make(chan struct{})
		go func() {
			server.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			log.G(ctx).Warn("graceful stop timed out; stopping hard")
			server.Stop()
		case sig := <-sigCh:
			log.G(ctx).WithField("signal", sig).Warn("second signal; stopping hard")
			server.Stop()
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// listen creates the unix-socket listener, ensuring the parent directory exists
// and clearing a stale socket left by a previous run.
func listen(address string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(address), 0700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	if fi, err := os.Lstat(address); err == nil && fi.Mode()&os.ModeSocket != 0 {
		if err := os.Remove(address); err != nil {
			return nil, fmt.Errorf("remove stale socket %q: %w", address, err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat socket %q: %w", address, err)
	}
	listener, err := net.Listen("unix", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %q: %w", address, err)
	}
	return listener, nil
}
