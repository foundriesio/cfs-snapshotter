// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"

	"github.com/foundriesio/ocicfs/pkg/ocicfs"

	"github.com/foundriesio/cfs-snapshotter/internal/snapshot"
)

// runCheck is the -check mode: one synchronous pass over everything the
// daemon needs, printed as one verdict line per prerequisite on w, exit code
// as the summary. Every prerequisite is inspected and reported whether or
// not an earlier one failed, so a single run shows the whole picture; a
// prerequisite that cannot be inspected at all says why instead of
// pretending a verdict. The checks are the ones the daemon itself enforces
// at startup, but as a plain command whose non-zero exit a script or a
// provisioning step can act on.
func runCheck(ctx context.Context, w io.Writer, cfg snapshot.Config, socketAddress string) int {
	admit := "preloaded"
	if cfg.AdmitAny {
		admit = "any"
	}
	workers := cfg.IngestWorkers
	if workers == 0 {
		workers = ocicfs.DefaultIngestWorkers
	}
	line(w, "checking runtime prerequisites (-admit=%s, -ingest-workers=%d):", admit, workers)
	failed := false
	report := func(name string, err error) {
		if err != nil {
			failed = true
			line(w, "%s: FAIL: %v", name, err)
			return
		}
		line(w, "%s: ok", name)
	}

	// Both modes mount from the store, so its fs-verity state is checked in
	// both; only admit-any mode builds into it and needs mkcomposefs.
	engine, err := ocicfs.New(ocicfs.Config{Root: cfg.OcicfsRoot})
	switch {
	case !cfg.AdmitAny:
		line(w, "1. mkcomposefs: skipped (preloaded mode builds nothing locally)")
	case err != nil:
		failed = true
		line(w, "1. mkcomposefs: FAIL: create ocicfs engine: %v", err)
	default:
		report("1. mkcomposefs", engine.PreflightTools())
	}
	store := fmt.Sprintf("2. fs-verity on %s", cfg.OcicfsRoot)
	if err != nil {
		failed = true
		line(w, "%s: FAIL: create ocicfs engine: %v", store, err)
	} else if reportStoreVerity(w, store, engine) {
		failed = true
	}
	r, reachable := containerdRound(ctx, cfg.ContainerdAddress, socketAddress)
	if !reachable {
		failed = true
		line(w, "3. containerd: FAIL: no answer at %s", cfg.ContainerdAddress)
		line(w, "4. containerd version 2.1+: not checked (containerd did not answer)")
		line(w, "5. containerd proxy_plugins wiring: not checked (containerd did not answer)")
		line(w, "6. enable_remote_snapshot_annotations export: not checked (containerd did not answer)")
		line(w, "7. containerd diff proxy_plugins wiring: not checked (containerd did not answer)")
	} else {
		line(w, "3. containerd: ok (version %s)", r.version)

		switch _, _, parsed := parseMajorMinor(r.version); {
		case !parsed:
			line(w, "4. containerd version 2.1+: unknown (unrecognized version %q); not enforced", r.version)
		case r.tooOld:
			failed = true
			line(w, "4. containerd version 2.1+: FAIL: %s is older than 2.1, whose transfer service does not forward the unpack handshake and image-identity annotations; every pull will fail", r.version)
		default:
			line(w, "4. containerd version 2.1+: ok")
		}

		switch {
		case r.introspectErr != nil:
			failed = true
			line(w, "5. containerd proxy_plugins wiring: FAIL: cannot list containerd's plugins: %v", r.introspectErr)
			line(w, "6. enable_remote_snapshot_annotations export: not checked (plugin listing failed)")
			line(w, "7. containerd diff proxy_plugins wiring: not checked (plugin listing failed)")
		case !r.wired:
			failed = true
			line(w, "5. containerd proxy_plugins wiring: FAIL: no proxy_plugins entry points at %s", socketAddress)
			line(w, "6. enable_remote_snapshot_annotations export: not checked (no wiring)")
			reportDiffer(w, r, socketAddress, cfg.AdmitAny)
		case !r.hasExport:
			failed = true
			line(w, "5. containerd proxy_plugins wiring: ok (proxy plugin %s)", r.entryID)
			line(w, "6. enable_remote_snapshot_annotations export: FAIL: missing enable_remote_snapshot_annotations = \"true\" under [proxy_plugins.%s.exports]; pulls arrive without image identity and are declined", r.entryID)
			reportDiffer(w, r, socketAddress, cfg.AdmitAny)
		default:
			line(w, "5. containerd proxy_plugins wiring: ok (proxy plugin %s)", r.entryID)
			line(w, "6. enable_remote_snapshot_annotations export: ok")
			reportDiffer(w, r, socketAddress, cfg.AdmitAny)
		}
		failed = failed || (cfg.AdmitAny && !r.differWired)
	}

	if failed {
		line(w, "check failed")
		return 1
	}
	line(w, "check passed")
	return 0
}

// reportStoreVerity prints whether this build runs the store under name with
// fs-verity, and reports whether that fails the check. A store the build runs
// without fs-verity passes: the daemon mounts it without requiring fs-verity,
// and the line says what that gives up. A store whose filesystem cannot
// enable fs-verity in a build that requires it fails, as does a probe that
// cannot run.
func reportStoreVerity(w io.Writer, name string, engine ocicfs.Engine) bool {
	state, err := engine.VerityState()
	switch {
	case err != nil:
		line(w, "%s: FAIL: %v", name, err)
		return true
	case state == "unsupported":
		line(w, "%s: FAIL: the filesystem cannot enable fs-verity, which this build (ocicfs_verity_required) requires", name)
		return true
	case state == "disabled":
		line(w, "%s: disabled (file contents are not checked on read)", name)
		return false
	}
	line(w, "%s: %s", name, state)
	return false
}

// reportDiffer checks that the proxy differ is registered. Missing wiring
// fails admit-any mode but only warns in preloaded mode, though archive loads
// still need it. Introspection cannot verify differ order or unpack selection.
func reportDiffer(w io.Writer, r probeResult, socketAddress string, admitAny bool) {
	if !r.differWired {
		if admitAny {
			line(w, "7. containerd diff proxy_plugins wiring: FAIL: no proxy_plugins entry of type \"diff\" points at %s; staged pulls fail at the layer apply", socketAddress)
		} else {
			line(w, "7. containerd diff proxy_plugins wiring: WARN: no proxy_plugins entry of type \"diff\" points at %s; docker load fails at the layer apply", socketAddress)
		}
		return
	}
	line(w, "7. containerd diff proxy_plugins wiring: ok (proxy plugin %s)", r.differID)
}

// line writes one verdict line to the report. The writer is stdout or a test
// buffer; a write error has nowhere better than the exit code to surface, so
// it is dropped.
func line(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", a...)
}

// containerdRound runs one probe round against containerd, bounded by ctx:
// an unreachable or silent containerd is reported, not waited for.
func containerdRound(ctx context.Context, containerdAddress, socketAddress string) (probeResult, bool) {
	conn, err := dialContainerd(containerdAddress)
	if err != nil {
		return probeResult{}, false
	}
	defer func() { _ = conn.Close() }()
	return probeOnce(ctx, conn, socketAddress)
}
