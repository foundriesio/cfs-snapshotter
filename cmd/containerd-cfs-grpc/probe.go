// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	introspectionapi "github.com/containerd/containerd/api/services/introspection/v1"
	versionapi "github.com/containerd/containerd/api/services/version/v1"
	"github.com/containerd/containerd/v2/pkg/dialer"
	"github.com/containerd/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/foundriesio/cfs-snapshotter/internal/snapshot"
)

// probeInterval paces the wait for containerd to come up. This daemon's unit
// is ordered before containerd's, so the socket usually appears within
// seconds of our own start.
const probeInterval = 5 * time.Second

// reprobeInterval paces the rounds after containerd has answered once, so an
// upgrade or configuration fix on the containerd side is noticed without
// restarting this daemon. It is a variable only for the tests, which cannot
// wait minutes for a second round.
var reprobeInterval = 5 * time.Minute

// snapshotterPluginType is the introspection type every snapshotter plugin,
// proxy ones included, is registered under.
const snapshotterPluginType = "io.containerd.snapshotter.v1"

// probeContainerd watches the containerd on containerdAddress and keeps
// checking whether it can actually drive this snapshotter:
//
//   - the server is containerd 2.1 or newer: older transfer services do not
//     forward the unpack handshake and image-identity annotations to proxy
//     snapshotters, so every pull is declined;
//   - a proxy snapshotter entry in containerd's config points at this
//     daemon's socket: without one containerd cannot reach this snapshotter;
//   - that entry exports enable_remote_snapshot_annotations = "true": without
//     it pulls arrive without image identity and are declined.
//
// Each round's verdict is pushed to systemd as the unit's status line, where
// systemctl status shows it next to "active (running)"; findings are logged
// when the verdict changes, with each error naming its remedy.
//
// The first definite verdict is binding: a containerd that cannot drive this
// snapshotter — too old, not wired, wiring without the annotations export —
// is an unmet prerequisite, reported as a non-nil return so the daemon exits
// and the unit lands in the failed state until the fix and a manual restart.
// After a healthy first verdict the rounds only inform: a live data plane
// with containers on it is never self-terminated over later drift, which the
// status line reports instead — and recovery from drift is likewise noticed
// without a restart.
func probeContainerd(ctx context.Context, containerdAddress, socketAddress string) error {
	conn, err := dialContainerd(containerdAddress)
	if err != nil {
		log.G(ctx).WithError(err).WithField("address", containerdAddress).
			Warn("containerd probe: cannot construct a client; skipping the configuration check")
		return nil
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("containerd probe: close connection")
		}
	}()

	last := ""
	binding := true
	for {
		r, ok := probeOnce(ctx, conn, socketAddress)
		if !ok {
			return nil
		}
		if r.introspectErr != nil {
			log.G(ctx).WithError(r.introspectErr).
				Warn("containerd probe: cannot list containerd's plugins; skipping the wiring check")
		}
		if status := r.status(); status != "" {
			if status != last {
				r.logVerdict(ctx, socketAddress)
				last = status
			}
			notify(ctx, "STATUS="+status)
			if binding {
				binding = false
				if r.tooOld || !r.wired || !r.hasExport {
					return fmt.Errorf("%w: %s", snapshot.ErrPrerequisites, strings.TrimPrefix(status, "degraded: "))
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(reprobeInterval):
		}
	}
}

// probeResult is one probe round's findings.
type probeResult struct {
	// version is what the server reported.
	version string
	// tooOld reports a version below the 2.1 floor.
	tooOld bool
	// introspectErr is the plugin listing failure that left the wiring
	// fields unknown.
	introspectErr error
	// wired reports a proxy snapshotter entry pointing at this daemon's
	// socket; entryID is its config name and hasExport whether it carries
	// the enable_remote_snapshot_annotations export.
	wired     bool
	entryID   string
	hasExport bool
}

// probeOnce runs one round against containerd, waiting for the server to
// answer first. ok is false only when ctx ended during that wait.
func probeOnce(ctx context.Context, conn *grpc.ClientConn, socketAddress string) (probeResult, bool) {
	var r probeResult
	version, ok := waitForVersion(ctx, versionapi.NewVersionClient(conn))
	if !ok {
		return r, false
	}
	r.version = version
	if major, minor, parsed := parseMajorMinor(version); parsed && (major < 2 || (major == 2 && minor < 1)) {
		r.tooOld = true
	}

	resp, err := introspectionapi.NewIntrospectionClient(conn).
		Plugins(ctx, &introspectionapi.PluginsRequest{})
	if err != nil {
		r.introspectErr = err
		return r, true
	}
	for _, p := range resp.Plugins {
		if p.Type == snapshotterPluginType && p.Exports["address"] == socketAddress {
			r.wired = true
			r.entryID = p.ID
			r.hasExport = p.Exports["enable_remote_snapshot_annotations"] == "true"
			break
		}
	}
	return r, true
}

// status renders the verdict for the unit's status line, the first problem
// winning. It is empty when introspection failed before any finding, so a
// transient listing error does not overwrite the last real verdict.
func (r probeResult) status() string {
	switch {
	case r.tooOld:
		return "degraded: containerd " + r.version + " is older than 2.1; pulls will fail"
	case r.introspectErr != nil:
		return ""
	case !r.wired:
		return "degraded: containerd has no proxy_plugins entry for this snapshotter's socket"
	case !r.hasExport:
		return "degraded: proxy_plugins." + r.entryID + " lacks enable_remote_snapshot_annotations; pulls will be declined"
	default:
		return "ok: wired to containerd " + r.version + " (proxy plugin " + r.entryID + ")"
	}
}

// logVerdict writes the round's findings to the journal, each error naming
// its remedy. The wiring fields are skipped when introspection failed, and
// correct wiring is not announced as success while the version already makes
// every pull fail.
func (r probeResult) logVerdict(ctx context.Context, socketAddress string) {
	if r.tooOld {
		log.G(ctx).WithField("version", r.version).
			Error("containerd probe: containerd is older than 2.1, whose transfer service does not forward the unpack handshake and image-identity annotations to proxy snapshotters; every pull through this snapshotter will fail")
	}
	if r.introspectErr != nil {
		return
	}
	switch {
	case !r.wired:
		log.G(ctx).WithField("socket", socketAddress).
			Error("containerd probe: no proxy snapshotter in containerd's config points at this daemon's socket; add the [proxy_plugins] entry (shipped as containerd-cfs.toml) and restart containerd")
	case !r.hasExport:
		log.G(ctx).WithField("plugin", r.entryID).
			Errorf("containerd probe: the proxy snapshotter entry does not export enable_remote_snapshot_annotations = \"true\", so pulls arrive without image identity and are declined; add it under [proxy_plugins.%s.exports] and restart containerd", r.entryID)
	case !r.tooOld:
		log.G(ctx).WithFields(log.Fields{"plugin": r.entryID, "version": r.version}).
			Info("containerd probe: containerd is wired to this snapshotter")
	}
}

// dialContainerd returns a client connection to containerd's gRPC socket;
// the connection is lazy, so errors surface on first use, not here.
func dialContainerd(address string) (*grpc.ClientConn, error) {
	return grpc.NewClient(dialer.DialAddress(address),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// waitForVersion retries the version request until containerd answers,
// returning false only when ctx ends first.
func waitForVersion(ctx context.Context, client versionapi.VersionClient) (string, bool) {
	for {
		resp, err := client.Version(ctx, &emptypb.Empty{})
		if err == nil {
			return resp.Version, true
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(probeInterval):
		}
	}
}

// parseMajorMinor reads the leading major.minor pair of a version string like
// "v2.2.3" or "1.7.24~ds1". ok is false when no such prefix exists; a caller
// that cannot parse a vendor's version string skips its check rather than
// guessing.
func parseMajorMinor(version string) (major, minor int, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	digits := parts[1]
	if i := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = digits[:i]
	}
	minor, err = strconv.Atoi(digits)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}
