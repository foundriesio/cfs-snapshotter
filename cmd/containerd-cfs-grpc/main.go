// SPDX-License-Identifier: Apache-2.0

// Command containerd-cfs-grpc is the cfs-snapshotter daemon: a containerd
// remote (proxy) snapshotter exposed over a gRPC unix socket. containerd is
// pointed at it via a [proxy_plugins.cfs] entry in its config.toml (see
// docs/running.md). At this stage the snapshotter methods are stubs that return
// "not implemented"; the wiring proves the proxy plumbing end to end.
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
	address      = flag.String("address", defaultAddress, "address for the snapshotter's gRPC server")
	rootDir      = flag.String("root", defaultRootDir, "path to the root directory for this snapshotter")
	logLevel     = flag.String("log-level", log.InfoLevel.String(), "logging level [trace, debug, info, warn, error, fatal, panic]")
	printVersion = flag.Bool("version", false, "print the version and exit")
)

func main() {
	flag.Parse()

	if *printVersion {
		fmt.Println("containerd-cfs-grpc", version.String())
		return
	}

	log.SetFormat(log.JSONFormat)
	if err := log.SetLevel(*logLevel); err != nil {
		fmt.Fprintln(os.Stderr, "invalid log level:", err)
		os.Exit(1)
	}

	ctx := log.WithLogger(context.Background(), log.L)
	if err := run(ctx, *address, *rootDir); err != nil {
		log.G(ctx).WithError(err).Fatal("cfs-snapshotter exited with error")
	}
}

// run builds the stub snapshotter, serves it over a gRPC unix socket, and
// blocks until SIGINT/SIGTERM, then shuts down gracefully.
func run(ctx context.Context, address, root string) error {
	sn, err := snapshot.NewSnapshotter(ctx, root)
	if err != nil {
		return fmt.Errorf("create snapshotter: %w", err)
	}
	defer sn.Close()

	listener, err := listen(address)
	if err != nil {
		return err
	}
	defer os.Remove(address)

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
		server.GracefulStop()
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
