// SPDX-License-Identifier: Apache-2.0

// Command containerd-cfs-grpc is the cfs-snapshotter daemon: a containerd
// remote (proxy) snapshotter exposed over a gRPC unix socket. containerd is
// pointed at it via a [proxy_plugins.cfs] entry in its config.toml (see
// docs/running.md). Image content is served from composefs images in the local
// knarr store.
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

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/log"
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
	logLevel           = flag.String("log-level", log.InfoLevel.String(), "logging level [trace, debug, info, warn, error, fatal, panic]")
	printVersion       = flag.Bool("version", false, "print the version and exit")
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
	}
	if err := run(ctx, *address, cfg); err != nil {
		log.G(ctx).WithError(err).Fatal("cfs-snapshotter exited with error")
	}
}

// run builds the snapshotter, serves it over a gRPC unix socket, and blocks
// until SIGINT/SIGTERM, then shuts down gracefully.
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

	errCh := make(chan error, 1)
	go func() {
		log.G(ctx).WithField("address", address).Info("serving cfs-snapshotter")
		if serveErr := server.Serve(listener); serveErr != nil {
			errCh <- fmt.Errorf("serve %q: %w", address, serveErr)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM)
	select {
	case sig := <-sigCh:
		log.G(ctx).WithField("signal", sig).Info("shutting down")
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
