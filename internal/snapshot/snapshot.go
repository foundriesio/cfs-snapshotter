// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package snapshot provides the cfs-snapshotter implementation of containerd's
// snapshots.Snapshotter interface, for use as a containerd remote (proxy)
// snapshotter.
//
// Snapshot metadata and lifecycle state live in a bolt-backed metadata store
// under the root; each directory-backed snapshot also owns a directory under
// root/snapshots holding its overlay upper and work directories. A snapshot
// mount is one flat overlay assembled by walking the parent chain: committed
// ancestors' upper directories stack as lowers, and the walk stops at the
// first image chain snapshot, whose data plane — the image's read-only EROFS
// metadata mount, with the knarr object directory stacked below it as a
// data-only lower requiring fs-verity — is the base lower. The snapshot's own
// upper receives its writes. Image content reaches the knarr store one of two
// ways: it is put there out of band, in which case every unpack handshake is
// answered as if the content were already present locally, or the runtime
// fetches it and the snapshotter, serving containerd's diff API as well,
// ingests each layer into the store as the unpacker applies it (see the
// calling contract below).
//
// # Calling contract
//
// Two callers drive this interface, distinguishable by request shape.
//
// Image unpack (containerd's unpacker, on behalf of an image pull) walks the
// layers bottom-up. For each layer i it asks whether the cumulative chain
// 1..i already exists as a committed snapshot and, if not, prepares and
// populates it:
//
//	Stat(chainID(1..i))                              -> ErrNotFound
//	Prepare(key="<ns>/extract-<rnd> <chainID(1..i)>",
//	        parent=chainID(1..i-1),
//	        labels["containerd.io/snapshot.ref"]=chainID(1..i))
//	  ... the caller fetches the layer and hands it, with the mounts, to
//	      the differ's Apply ...
//	Commit(name=chainID(1..i), key=<the extract key>)
//	Stat(chainID(1..i))                              -> confirmation
//
// Committed layer snapshots are always named by the cumulative chainID of
// layers 1..i, never by a single layer's digest, so the last handshake of a
// pull carries the whole image's chainID.
//
// The snapshot.ref label marks a Prepare as this handshake: it names the
// committed snapshot the unpacker actually wants. A snapshotter that can
// produce that snapshot without the caller fetching anything answers
// ErrAlreadyExists, which skips the fetch/apply/Commit round for that layer.
// When it does, the pull completes with zero layer fetches — the caller still
// downloads the image metadata (index, manifest, config, etc.) — and the
// mounts the container flow later receives compose the image's EROFS metadata
// and knarr objects as the base of one flat overlay (see mounts).
//
// A snapshotter that stages its pulls instead answers the handshake with a
// staging snapshot: a metadata record whose one mount is a placeholder no
// kernel can mount. Only this snapshotter's own differ (see Apply) consumes
// it, feeding the fetched layer straight from containerd's content store into
// the knarr store's build of the image; a containerd that routes the layer to
// another differ fails at the placeholder instead of extracting into nothing.
// Commit then files the record; the chain's last Apply, finishing the build,
// pins the image, and its Commit files the labels that name that hold.
//
// Container creation (the docker daemon, via containerd) prepares two
// snapshots on top of the image's full chain:
//
//	Prepare(key=<id>-init, parent=chainID(1..n))     -> mounts
//	  ... docker seeds runtime files: /etc/hostname, /etc/hosts, ... ...
//	Commit(name=<id>-init, key=<id>-init)
//	Prepare(key=<id>, parent=<id>-init)              -> the rootfs mounts
//
// The committed init layer keeps those runtime files out of both the image
// and the container's own diff. Neither call carries snapshot.ref: the
// absence of that label is what separates a snapshot somebody will mount and
// write to from unpack bookkeeping.
//
// Around both flows, Stat, Usage, and Walk serve size accounting,
// parent-chain resolution, and the metadata garbage collector — Stat in
// particular fires repeatedly per committed name — and Remove tears
// snapshots down children-first.
package snapshot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/foundriesio/knarr/pkg/knarr"
	"github.com/moby/sys/mountinfo"
	"github.com/opencontainers/go-digest"

	"github.com/containerd/continuity/fs"
)

// intermediateSnapshotLabel marks a Prepare call as the unpack handshake: its
// value is the name (a cumulative chainID) under which the caller wants the
// layer chain to exist as a committed snapshot. Such snapshots are
// intermediate — they are only ever parents of other snapshots, never mounted
// themselves — and start out virtual: pure metadata records whose data plane
// is mounted only if and when a descendant snapshot needs the image.
//
// imageRefLabel carries the image reference the runtime pulled. The handshake
// admits a layer chain only when this reference resolves to a locally stored
// image containing that chain, so a pull of an image the store does not hold
// is declined at pull time instead of being silently answered.
//
// manifestDigestLabel carries the digest of the manifest the runtime resolved
// the reference to. It is the trust anchor a staged chain is built against: the
// reference names the image, the digest fixes which image it is.
//
// chainMemberLabel marks a snapshot as standing for a layer chain of an image
// in the knarr store. It is what ends a mount's parent walk: the labeled
// snapshot's image is the base of the overlay, so nothing below it is
// consulted.
//
// stagingLabel marks a snapshot of a staged pull: a metadata record whose
// layer reaches the knarr store through the differ rather than through a
// directory of its own, and whose only mount is the staging placeholder.
//
// leafLabel marks a staging record whose layer is the image's last. The
// differ writes it once it has resolved the layer against the manifest, so
// Commit learns it pins the image from the record alone, without reading
// anything.
//
// rootDigestLabel records the root digest a snapshot's image is held under, so
// the hold is released against the digest that was taken rather than against a
// reference a tag may have moved in the meantime.
//
// holderLabel records the holder name the record took that hold under, so a
// record releases its own hold and not another record's.
//
// The last five are private to this snapshotter. containerd hands a backend
// Commit the containerd.io/snapshot.ref label and the containerd.io/snapshot/*
// prefix and drops everything else, so Commit carries them over from the
// active record itself.
const (
	intermediateSnapshotLabel = "containerd.io/snapshot.ref"
	imageRefLabel             = "containerd.io/snapshot/cri.image-ref"
	manifestDigestLabel       = "containerd.io/snapshot/cri.manifest-digest"
	privateLabelPrefix        = "io.foundries.cfs/"
	chainMemberLabel          = privateLabelPrefix + "chain-member"
	stagingLabel              = privateLabelPrefix + "staging"
	leafLabel                 = privateLabelPrefix + "leaf"
	rootDigestLabel           = privateLabelPrefix + "root-digest"
	holderLabel               = privateLabelPrefix + "holder"
)

// recordHolder names the knarr hold a single snapshot record takes. Every image
// the snapshotter hands containerd a record for is held until containerd
// forgets that record, so a knarr remove cannot take content away from
// underneath a runtime that is counting on it. One name per record, rather than
// one for the snapshotter, is what keeps two records standing for the same
// image independent: each takes its own hold, and the image is let go only once
// the last of them is removed.
//
// The name is a hash of the record name rather than the name itself because a
// holder may only carry [a-z0-9._-], and every record containerd names carries
// a path separator and a digest colon. The record keeps the result in
// holderLabel, so a release names the hold that record took rather than
// re-deriving it.
func recordHolder(name string) string {
	return fmt.Sprintf("%s%x", holderPrefix, sha256.Sum256([]byte(name)))
}

// holderPrefix starts every holder name this snapshotter takes, so a holder
// of its own can be told from an agent's when holds are reconciled at
// startup.
const holderPrefix = "cfs-"

// gcRetryInterval paces the collections retried after the knarr store
// reported itself busy.
var gcRetryInterval = 30 * time.Second

// snapshotter is the cfs-snapshotter implementation of snapshots.Snapshotter.
type snapshotter struct {
	// root is the snapshotter's root directory.
	root string
	// objectsDir is the knarr store's backing-object directory.
	objectsDir string
	// ms is the bolt-backed metadata store holding snapshot records.
	ms *storage.MetaStore
	// engine provides access to images in the local knarr store.
	engine engineAPI
	// blobs reads image blobs out of containerd's content store, the source a
	// staged chain is built from.
	blobs *contentBlobs
	// mounted reports whether a path is a mount point; it is mountinfo.Mounted
	// in production and is replaced in tests.
	mounted func(string) (bool, error)
	// allowUnstagedPulls stages every pull instead of admitting only images
	// already in the knarr store.
	allowUnstagedPulls bool
	// mountMu serializes shared chain mount creation and teardown.
	mountMu sync.Mutex
	// builds are the image builds in progress, one per staged pull, each
	// fed a layer at a time as the differ applies the chain. buildMu guards
	// the map and the start of a build, and is released before a build is
	// fed: a blocked ingest holds up nothing but its own pull, while a pull
	// still starting its build holds up every other pull's map access. It is
	// never held across a write transaction, for the reason given at mounts.
	buildMu sync.Mutex
	builds  map[buildKey]*openBuild
	// finishing are the builds whose last layer is being applied, keyed by
	// that layer's staging snapshot: out of builds, so a duplicate pull
	// reaching the leaf starts a build of its own, but findable by the
	// record containerd removes if it unwinds the pull meanwhile. buildMu
	// guards it too.
	finishing map[string]*openBuild
	// dirty records that a hold was released, or a build ended, since the
	// store was last collected, so a collection has something to reclaim.
	// Collections run under reclaimMu — from Cleanup, on containerd's cycle,
	// and from the reclaim goroutine — on lifetime, the context Close
	// cancels before it waits for that goroutine. Neither channel is ever
	// closed: kick wakes the goroutine to collect now, retry to collect once
	// the interval has passed after a busy store.
	//
	// releases counts the holds Remove has released, and covered is the count
	// the last completed collection had seen when it began, so a cleanup with
	// no release left to cover returns without waiting for a collection that
	// is running for another reason.
	reclaimMu sync.Mutex
	dirty     atomic.Bool
	releases  atomic.Uint64
	covered   atomic.Uint64
	kick      chan struct{}
	retry     chan struct{}
	lifetime  context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// Compile-time assertion that *snapshotter collects on containerd's cleanup
// cycle.
var _ snapshots.Cleaner = (*snapshotter)(nil)

// Compile-time assertion that *snapshotter satisfies the containerd interface.
var _ snapshots.Snapshotter = (*snapshotter)(nil)

// Snapshotter is what NewSnapshotter returns: containerd's snapshotter
// interface and the differ that applies a staged pull's layers, served by
// one object because the differ works on the snapshotter's own records.
type Snapshotter interface {
	snapshots.Snapshotter
	snapshots.Cleaner
	diff.Applier
}

// ErrPrerequisites marks a failure whose cause is an unmet runtime
// prerequisite — a host or containerd misconfiguration an administrator must
// fix — rather than a fault in the snapshotter. The daemon exits with a
// dedicated status on it, which the packaged unit excludes from automatic
// restart so the service lands in the failed state until the fix and a
// manual restart.
var ErrPrerequisites = errors.New("runtime prerequisites not met")

// Config is the snapshotter's configuration.
type Config struct {
	// Root is the snapshotter's own root directory.
	Root string
	// KnarrRoot is the root of the knarr store the images are served from.
	KnarrRoot string
	// AllowUnstagedPulls selects the machine's work mode. False admits only
	// images already in the knarr store, put there out of band, and declines
	// every other pull. True stages every pull — even of an image the store
	// already holds — and builds it into the store once the chain is
	// complete, so the runtime fetches and keeps each pulled image's layers
	// in its own content store.
	AllowUnstagedPulls bool
	// ContainerdAddress is the address of containerd's gRPC socket, whose
	// content store the layers of a staged pull are read back from.
	ContainerdAddress string
	// IngestWorkers is the number of layers the knarr store ingests at once
	// across every staged pull in progress. 0 selects knarr's default.
	IngestWorkers int
}

// NewSnapshotter returns a Snapshotter configured by cfg, serving composefs
// images from the knarr store at cfg.KnarrRoot.
func NewSnapshotter(ctx context.Context, cfg Config) (Snapshotter, error) {
	engine, err := knarr.New(knarr.Config{Root: cfg.KnarrRoot, IngestWorkers: cfg.IngestWorkers})
	if err != nil {
		return nil, fmt.Errorf("create knarr engine: %w", err)
	}
	// Staged mode builds every pull into the knarr store, so a host missing
	// mkcomposefs or fs-verity fails every pull; refusing to start reports
	// the misconfiguration at install time instead of at the first image.
	// Out-of-band mode builds nothing locally and needs neither.
	if cfg.AllowUnstagedPulls {
		if err := errors.Join(engine.PreflightTools(), engine.PreflightStore()); err != nil {
			return nil, fmt.Errorf("staged-pull prerequisites: %w: %w", ErrPrerequisites, err)
		}
	}
	return newSnapshotter(ctx, cfg, engine)
}

func newSnapshotter(ctx context.Context, cfg Config, engine engineAPI) (*snapshotter, error) {
	root := cfg.Root
	if root == "" {
		return nil, fmt.Errorf("cfs snapshotter: root directory must not be empty: %w", errdefs.ErrInvalidArgument)
	}
	if strings.ContainsAny(root, ":,") {
		return nil, fmt.Errorf("cfs snapshotter: root directory must not contain ':' or ',' (paths under it become overlay mount options): %w", errdefs.ErrInvalidArgument)
	}
	objectsDir, err := engine.ObjectsDir()
	if err != nil {
		return nil, fmt.Errorf("cfs snapshotter: resolve knarr object directory: %w", err)
	}
	if strings.ContainsAny(objectsDir, ":,") {
		return nil, fmt.Errorf("cfs snapshotter: knarr object directory must not contain ':' or ',' (it becomes an overlay mount option): %w", errdefs.ErrInvalidArgument)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("cfs snapshotter: create root: %w", err)
	}
	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, fmt.Errorf("cfs snapshotter: open metadata store: %w", err)
	}
	if err := os.Mkdir(filepath.Join(root, "snapshots"), 0700); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("cfs snapshotter: create snapshots dir: %w", err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	s := &snapshotter{
		root:               root,
		objectsDir:         objectsDir,
		ms:                 ms,
		engine:             engine,
		blobs:              newContentBlobs(cfg.ContainerdAddress),
		mounted:            mountinfo.Mounted,
		allowUnstagedPulls: cfg.AllowUnstagedPulls,
		builds:             make(map[buildKey]*openBuild),
		finishing:          make(map[string]*openBuild),
		kick:               make(chan struct{}, 1),
		retry:              make(chan struct{}, 1),
		lifetime:           lifetime,
		cancel:             cancel,
	}
	// A previous run may have left holds behind — a build finished but never
	// committed, a record that never landed — and the flag that says there is
	// something to collect does not survive a restart; reconcile, then have
	// the first collection run unconditionally.
	if err := s.reconcileHolds(ctx); err != nil {
		log.G(ctx).WithError(err).Error("cfs snapshotter: cannot reconcile the store's holds; leaked holds stay until the next start")
	}
	s.dirty.Store(true)
	s.wg.Add(1)
	go s.reclaim()
	return s, nil
}

// reconcileHolds releases every hold this snapshotter took that no record of
// its owns any more. The records are the intent: a committed leaf carries the
// holder its image is held under, and so does the staging record of a leaf
// applied but not yet committed; a holder of this snapshotter's that no
// record names was stranded by a crash between the hold and its record, or
// by a build that finished for a pull the runtime then dropped. Holders that
// are not this snapshotter's are left alone.
func (s *snapshotter) reconcileHolds(ctx context.Context) error {
	owned := map[string]struct{}{}
	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, func(_ context.Context, info snapshots.Info) error {
			if holder := info.Labels[holderLabel]; holder != "" {
				owned[holder] = struct{}{}
			}
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("walk snapshot records: %w", err)
	}
	held, err := s.engine.Holds(ctx)
	if err != nil {
		return err
	}
	for root, holders := range held {
		for _, holder := range holders {
			if !strings.HasPrefix(holder, holderPrefix) {
				continue
			}
			if _, ok := owned[holder]; ok {
				continue
			}
			log.G(ctx).WithFields(log.Fields{"digest": root, "holder": holder}).
				Info("cfs snapshotter: releasing a hold no snapshot record owns")
			s.releaseHold(ctx, root.String(), holder)
		}
	}
	return nil
}

// Cleanup collects the knarr store once the runtime has dropped an image.
// containerd calls it at the end of each of its own collection cycles, after
// the Remove of every record that cycle pruned, and — for an image removal
// the client asked to be synchronous, as docker's is — waits for it, so the
// store is reclaimed by the time the client's command returns. The
// collection runs on the snapshotter's own context: the RPC's only waits.
// A cycle that released no hold since the last completed collection, such as
// a container's removal, returns at once instead of waiting for a collection
// the snapshotter is running for another reason.
func (s *snapshotter) Cleanup(ctx context.Context) error {
	log.G(ctx).Trace("cfs snapshotter: cleanup")
	if s.covered.Load() >= s.releases.Load() {
		return nil
	}
	return s.collect()
}

// collectSoon marks the store to be collected and wakes the reclaim
// goroutine to do it. A build that ended is what calls it: finished or not,
// the build wrote every file of every layer it saw, and the files a later
// layer replaced or removed are objects no image references.
func (s *snapshotter) collectSoon() {
	s.dirty.Store(true)
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// collect runs one collection of the knarr store if a hold was released, or
// a build ended, since the last one. A store busy with a pull, hold or mount
// defers the collection to the reclaim goroutine — the removal that made it
// necessary has already happened, only the reclaiming waits — while any
// other failure is reported and not retried: a store the collector cannot
// enumerate fails the same way every time, and is fixed by hand, not by
// re-walking it.
func (s *snapshotter) collect() error {
	s.reclaimMu.Lock()
	defer s.reclaimMu.Unlock()
	if !s.dirty.Swap(false) {
		return nil
	}
	releases := s.releases.Load()
	report, err := s.engine.GC(s.lifetime)
	switch {
	case errors.Is(err, knarr.ErrStoreBusy):
		s.dirty.Store(true)
		select {
		case s.retry <- struct{}{}:
		default:
		}
		log.L.WithError(err).Debug("cfs snapshotter: store busy, collection deferred")
		return nil
	case s.lifetime.Err() != nil:
		return nil
	case err != nil:
		s.covered.Store(releases)
		log.L.WithError(err).Error("cfs snapshotter: store collection failed")
		return fmt.Errorf("collect the knarr store: %w", err)
	}
	s.covered.Store(releases)
	entry := log.L.WithFields(log.Fields{
		"objects":  report.ObjectsRemoved,
		"bytes":    report.BytesFreed,
		"bindings": report.BindingsRemoved,
		"in_use":   report.InUseSkipped,
	})
	if report.ObjectsRemoved > 0 || report.BindingsRemoved > 0 {
		entry.Info("cfs snapshotter: collected the store")
	} else {
		entry.Debug("cfs snapshotter: collected the store")
	}
	return nil
}

// reclaim is the collector's goroutine: it runs the first collection of the
// snapshotter's life, then one on every kick, and one after each interval a
// collection asked for by finding the store busy. It exits when the
// snapshotter closes.
func (s *snapshotter) reclaim() {
	defer s.wg.Done()
	_ = s.collect()
	for {
		select {
		case <-s.lifetime.Done():
			return
		case <-s.kick:
		case <-s.retry:
			select {
			case <-s.lifetime.Done():
				return
			case <-time.After(gcRetryInterval):
			}
		}
		_ = s.collect()
	}
}

// engineAPI is the subset of knarr.Engine the snapshotter uses: reading the
// local store (Inspect, ObjectsDir), mounting an image chain from it
// (MountChain, Unmount), building an image out of layers the runtime fetches
// (NewBuild), pinning what it hands containerd for as long as containerd
// references it (Hold, Release, and Holds to reconcile them at startup), and
// reclaiming what nothing pins any more (GC). Building happens in the
// differ's Apply, a layer at a time as the runtime fetches the chain; a
// Prepare still never reaches for content over the network.
// Publish and Remove are deliberately excluded — the snapshotter publishes
// nothing and forgets nobody's reference — so snapshotter code cannot
// casually reach for them: the one thing that deletes image data is the
// collector, which reclaims only what no reference and no hold wants. The
// narrow surface is also the seam the tests fake.
type engineAPI interface {
	Inspect(ctx context.Context, ref string) (*knarr.ImageInfo, error)
	NewBuild(ctx context.Context, ref string, opts ...knarr.PullOption) (knarr.Build, error)
	Hold(ctx context.Context, ref, holder string) error
	Release(ctx context.Context, ref, holder string) error
	Holds(ctx context.Context) (map[digest.Digest][]string, error)
	MountChain(ctx context.Context, chainID, target string, opts ...knarr.MountOption) error
	Unmount(ctx context.Context, target string) error
	ObjectsDir() (string, error)
	GC(ctx context.Context, opts ...knarr.GCOption) (*knarr.GCReport, error)
}

// Stat returns the info for an active or committed snapshot by key. It is the
// hottest read on the interface: the unpacker probes each chainID before its
// Prepare handshake and confirms it after, usage accounting re-resolves every
// chain member, and any parent-chain walk re-Stats each ancestor — so one
// committed name is Stat'd many times over its life. It must stay a cheap
// metadata lookup.
func (s *snapshotter) Stat(ctx context.Context, key string) (info snapshots.Info, err error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: stat")
	err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, info, _, err = storage.GetInfo(ctx, key)
		return err
	})
	if err != nil {
		return snapshots.Info{}, err
	}
	return info, nil
}

// Update modifies the labels and timestamps of an existing snapshot's info. The
// fieldpaths, when given, restrict which fields are written. In practice the
// caller is containerd's garbage collector flipping retention labels
// (containerd.io/gc.*) on committed snapshots.
//
// The snapshotter's own labels survive the update on their own account: the
// caller strips labels outside the containerd.io namespace from the map it
// hands down, so a whole-map label write would silently take this
// snapshotter's markers off the record — and with them the classification its
// mounts depend on. Whatever the update leaves missing of the record's prior
// private labels is put back.
func (s *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (_ snapshots.Info, err error) {
	log.G(ctx).WithFields(log.Fields{
		"name":       info.Name,
		"kind":       info.Kind,
		"labels":     info.Labels,
		"fieldpaths": fieldpaths,
	}).Trace("cfs snapshotter: update")
	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, prior, _, err := storage.GetInfo(ctx, info.Name)
		if err != nil {
			return err
		}
		info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
		if err != nil {
			return err
		}
		restore := snapshots.Info{Name: info.Name, Labels: info.Labels}
		var lost []string
		for k, v := range privateLabels(prior.Labels) {
			if _, ok := info.Labels[k]; ok {
				continue
			}
			if restore.Labels == nil {
				restore.Labels = map[string]string{}
			}
			restore.Labels[k] = v
			lost = append(lost, "labels."+k)
		}
		if len(lost) == 0 {
			return nil
		}
		info, err = storage.UpdateInfo(ctx, restore, lost...)
		return err
	})
	if err != nil {
		return snapshots.Info{}, err
	}
	return info, nil
}

// Usage reports the disk usage of the snapshot identified by key. An active
// snapshot is measured from its directory. A committed one is priced from what
// it actually costs now rather than from what it cost when it was committed,
// because neither figure can be rewritten afterwards: a snapshot standing for
// an image costs that image's composefs blob, and a staged record, whose
// layer lives in the knarr store, costs nothing. Only a record that is
// neither — a container's own layer — reports the usage recorded at commit
// time. After an unpack the caller queries every member of the image's chain
// in turn to price the image, so a Usage(chainID)+Stat(chainID) pair per
// layer is the normal post-pull pattern.
func (s *snapshotter) Usage(ctx context.Context, key string) (usage snapshots.Usage, err error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: usage")
	var (
		id   string
		info snapshots.Info
	)
	err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, info, usage, err = storage.GetInfo(ctx, key)
		return err
	})
	if err != nil {
		return snapshots.Usage{}, err
	}
	if info.Labels[stagingLabel] != "" {
		return snapshots.Usage{}, nil
	}
	dir := s.getSnapshotDir(id)
	if info.Kind == snapshots.KindActive {
		du, err := fs.DiskUsage(ctx, dir)
		if err != nil {
			return snapshots.Usage{}, err
		}
		return snapshots.Usage(du), nil
	}
	if root := info.Labels[rootDigestLabel]; root != "" {
		image, err := s.engine.Inspect(ctx, root)
		if err != nil {
			return snapshots.Usage{}, fmt.Errorf("inspect image %s to price snapshot %q: %w", root, key, err)
		}
		return snapshots.Usage{Size: image.Composefs.Size}, nil
	}
	if !pathExists(filepath.Join(dir, "upper")) {
		return snapshots.Usage{}, nil
	}
	return usage, nil
}

// Mounts returns the mounts for the snapshot identified by key. Consumers
// re-fetch mount specs rather than caching what Prepare returned — a runtime
// typically calls Mounts just before starting a container — so the specs must
// stay reproducible for the lifetime of the snapshot.
func (s *snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: mounts")
	return s.mounts(ctx, key)
}

// Prepare records a new active snapshot under key, descending from parent,
// and returns its mounts. Both call shapes from the package contract land
// here, and the snapshot.ref label decides the work: a handshake Prepare for
// a layer chain the knarr store holds needs only a metadata record — created
// and committed virtually in one transaction, no directory ever provisioned —
// and is answered with ErrAlreadyExists instead of mounts; one for a chain
// the store does not hold is declined. Where staging is allowed, every
// handshake instead gets a staging snapshot — a record with the placeholder
// mount the differ applies the layer through — whether or not the store
// holds its image. Everything else gets a directory-backed active snapshot
// and overlay mounts composing the image metadata, the knarr objects, the
// uppers of its committed ancestors, and its own upper (see mounts).
func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithFields(log.Fields{"key": key, "parent": parent}).
		WithFields(optFields(opts)).Trace("cfs snapshotter: prepare")
	var base snapshots.Info
	for _, opt := range opts {
		if err := opt(&base); err != nil {
			return nil, fmt.Errorf("prepare options: %w", err)
		}
	}

	if name, ok := base.Labels[intermediateSnapshotLabel]; ok {
		// This Prepare is one step of the image-unpack handshake: while a
		// pull walks the layers bottom-up, the unpacker prepares a throwaway
		// snapshot per layer, labeled with the name (the cumulative chainID)
		// it wants that layer chain to exist under, before fetching the
		// layer. Committing the intermediate virtual snapshot under that name
		// and answering ErrAlreadyExists tells the caller the chain is
		// already present locally: it skips the layer's fetch, apply, and
		// commit, and moves on to the next layer — the nil mounts are never
		// looked at. This answer, repeated per layer, is how a pull against
		// pre-fetched content completes with zero layer downloads; the caller
		// still fetches the image metadata (index, manifest, config, etc.).
		err := s.createAndCommitIntermediateVirtualSnapshot(ctx, key, parent, name, base.Labels, opts)
		if !errors.Is(err, errStagePull) {
			return nil, err
		}
		// This daemon stages its pulls, so this Prepare instead returns a
		// staging snapshot — the handshake's "not present" answer: the
		// caller then fetches the layer, hands it to the differ, and
		// commits the record itself.
		return s.createStagingSnapshot(ctx, key, parent,
			append(opts, snapshots.WithLabels(map[string]string{stagingLabel: "true"})))
	}

	// Everything else asks for a real writable snapshot whose mounts will
	// actually be executed, for example docker's init layer or the
	// container rootfs.
	return s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

// View records a new read-only snapshot under key, descending from parent, and
// returns its mounts. Views are how a caller inspects committed content
// without creating a writable layer — containerd's differ and image export
// use them; the container flows never do.
func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithFields(log.Fields{"key": key, "parent": parent}).
		WithFields(optFields(opts)).Trace("cfs snapshotter: view")
	return s.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

// Commit moves the active snapshot identified by key to a committed snapshot
// named name, recording its disk usage. The name encodes who is committing:
// the unpacker commits an applied layer under its cumulative chainID, and
// the docker daemon commits the init layer it populated under
// <container-id>-init so it can parent the container rootfs on it. A
// committed snapshot is immutable and exists to be a parent. The
// snapshotter's own labels move across with it: the caller passes on only the
// labels containerd defines, so a marker this snapshotter put on the active
// record would otherwise be lost exactly when the record becomes something
// other snapshots descend from.
//
// Committing a staged snapshot files the record of a layer the differ has
// already fed into the knarr store. containerd calls it inside its own
// metadata transaction, so everything it needs is on the record: the chain
// it stands for is its snapshot.ref label — never the name, which is
// containerd's own bookkeeping — and whether that chain is the image's last
// is the leaf marker the differ left. Everything but the last layer is filed
// as it stands; the last one's record becomes an image chain member — from
// which instant a mount over it composes the image — carrying the image's
// identity and the holder the build that finished the image holds it under,
// both left on the staging record by the differ. The hold keeps the image
// for as long as containerd references the record, which releases it when
// removed; Commit itself touches the store not at all. A staged Commit needs
// no build to be open: after a restart between a layer's Apply and its
// Commit, the next Apply starts a build that catches up from the content
// store.
func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	log.G(ctx).WithFields(log.Fields{"name": name, "key": key}).
		WithFields(optFields(opts)).Trace("cfs snapshotter: commit")
	var labels map[string]string
	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, info, _, err := storage.GetInfo(ctx, key)
		labels = info.Labels
		return err
	})
	if err != nil {
		return err
	}
	if labels[stagingLabel] == "" {
		return s.commitRecord(ctx, name, key, nil, opts)
	}

	start := time.Now()
	leaf := labels[leafLabel] != ""
	var private map[string]string
	if leaf {
		private = map[string]string{
			chainMemberLabel: "true",
			rootDigestLabel:  labels[rootDigestLabel],
			holderLabel:      labels[holderLabel],
		}
	}
	if err := s.commitRecord(ctx, name, key, private, opts); err != nil {
		return err
	}
	log.G(ctx).WithFields(log.Fields{
		"name":    name,
		"chain":   labels[intermediateSnapshotLabel],
		"leaf":    leaf,
		"elapsed": time.Since(start).Seconds(),
	}).Info("cfs snapshotter: committed staged layer")
	return nil
}

// Remove abandons the snapshot identified by key, dropping its metadata record
// and the snapshot directory. The directory is renamed out of the way inside
// the transaction so a failure can restore it. Removal is driven by
// containerd's garbage collector children-first — a container's rootfs
// snapshot goes before its init layer, and image chains unwind leaf-first
// once no image references them.
//
// A record that pinned an image in the knarr store releases it here, against
// the root digest and the holder name the record itself carries rather than any
// reference that may have moved since. The release names that record's own
// hold, so removing a record that never took one, or one of several records
// standing for the same image, leaves the other holds in place; the image is
// let go once containerd has forgotten the last of them. A staged record of a
// pull still in progress belongs to a pull the runtime is unwinding, and its
// build is abandoned along with it (see closeStagedBuild); the last layer's
// staging record carries the holder its build holds the image under, so an
// image finished but never committed is released the same way.
func (s *snapshotter) Remove(ctx context.Context, key string) (err error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: remove")
	var (
		renamed, path string
		labels        map[string]string
		restore       bool
	)
	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, info, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot info: %w", err)
		}
		labels = info.Labels
		id, _, err := storage.Remove(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to remove: %w", err)
		}
		path = s.getSnapshotDir(id)
		// path may not exist: virtual snapshots (see
		// createAndCommitIntermediateVirtualSnapshot) never get a directory.
		// unmountDirIfMounted and the rename below both treat that as a no-op.
		if err := s.unmountDirIfMounted(ctx, path); err != nil {
			return err
		}
		// Rename now (one fast, atomic syscall) and delete after the
		// transaction closes, so the slow recursive delete doesn't hold the
		// metadata store's write lock.
		renamed = filepath.Join(s.root, "snapshots", "rm-"+id)
		if err = os.Rename(path, renamed); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("failed to rename: %w", err)
			}
			renamed = "" // path never existed; nothing to restore or clean up below
		}
		restore = true
		return nil
	})
	if err != nil {
		if renamed != "" && restore {
			if err1 := os.Rename(renamed, path); err1 != nil {
				// May cause inconsistent data on disk.
				log.G(ctx).WithError(err1).WithField("path", renamed).Error("failed to rename after failed remove")
			}
		}
		return err
	}
	if renamed != "" {
		if err := os.RemoveAll(renamed); err != nil {
			// Must be cleaned up; any "rm-*" left behind can be removed when no
			// transactions are active.
			log.G(ctx).WithError(err).WithField("path", renamed).Warn("failed to remove snapshot directory")
		}
	}
	// The pull's build, if one is still running, is closed before the hold is
	// released: a build finishing its image takes the hold as it completes,
	// and a release before that would find nothing and leave the hold that
	// lands after it with nothing to release it.
	s.closeStagedBuild(ctx, key, labels)
	// A record that took a hold carries both labels, written together. What
	// the release leaves unwanted is the next collection's to reclaim.
	if root := labels[rootDigestLabel]; root != "" {
		s.releaseHold(ctx, root, labels[holderLabel])
		// Counted before the store is marked, so a collection that reads the
		// count too early to cover this release leaves the mark for the next.
		s.releases.Add(1)
		s.dirty.Store(true)
	}
	return nil
}

// releaseHold drops one of the knarr store's holds on root, logging a failure
// rather than reporting it: every caller has already passed the point where it
// could undo its own work, so what a failure leaves behind is an image held
// longer than it need be.
func (s *snapshotter) releaseHold(ctx context.Context, root, holder string) {
	if err := s.engine.Release(ctx, root, holder); err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"digest": root, "holder": holder}).
			Warn("failed to release the image hold")
	}
}

// Walk invokes fn for each committed snapshot matching the given filters. The
// garbage collector uses it to enumerate what exists, so everything recorded
// in the metadata store — including bookkeeping snapshots — must surface here
// or it leaks.
func (s *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	log.G(ctx).WithField("filters", filters).Trace("cfs snapshotter: walk")
	return s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, fn, filters...)
	})
}

// Close stops the store collector, abandons the image builds in progress and
// releases the snapshotter's metadata store and the containerd connection it
// reads image blobs over. It is safe to call on an already-closed
// snapshotter.
func (s *snapshotter) Close() error {
	log.L.Trace("cfs snapshotter: close")
	s.cancel()
	s.wg.Wait()
	closeBuilds(s.takeBuilds(func(buildKey, *openBuild) bool { return true }))
	closeBuilds(s.takeFinishing(""))
	err := s.blobs.Close()
	if s.ms != nil {
		err = errors.Join(err, s.ms.Close())
	}
	return err
}

// optFields renders the snapshot info fields the given opts set. An Opt
// mutates an Info, so applying the opts to a zero Info exposes exactly what
// the caller attached — in practice the labels, which carry the
// remote-snapshotter protocol (the containerd.io/snapshot.ref name and the
// containerd.io/snapshot/* chain labels) and are otherwise invisible until
// they land in the metadata store.
func optFields(opts []snapshots.Opt) log.Fields {
	var info snapshots.Info
	for _, opt := range opts {
		if err := opt(&info); err != nil {
			return log.Fields{"optserror": err.Error()}
		}
	}
	fields := log.Fields{}
	if info.Name != "" {
		fields["optname"] = info.Name
	}
	if info.Parent != "" {
		fields["optparent"] = info.Parent
	}
	if len(info.Labels) > 0 {
		fields["labels"] = info.Labels
	}
	return fields
}

// privateLabels picks this snapshotter's own labels out of a snapshot's label
// set, returning nil when there are none.
func privateLabels(labels map[string]string) map[string]string {
	var private map[string]string
	for k, v := range labels {
		if !strings.HasPrefix(k, privateLabelPrefix) {
			continue
		}
		if private == nil {
			private = make(map[string]string, len(labels))
		}
		private[k] = v
	}
	return private
}

// errStagePull reports that the unpack handshake arrived while pulls are
// staged. Prepare answers it with a staging snapshot instead of a virtual one,
// so the caller fetches and applies the layer itself.
var errStagePull = errors.New("stage the pull")

// createAndCommitIntermediateVirtualSnapshot answers the unpack handshake when
// the caller names a locally stored image containing the requested chain. The
// snapshot is recorded as pure metadata — created and committed under the
// requested name, with the caller's labels, the chain-member marker, and zero
// usage, in one transaction — and ErrAlreadyExists is reported so the caller
// skips fetching and applying the layer. Nothing mounts an intermediate
// snapshot, so no directory is provisioned, and a concurrent handshake that
// loses the commit race rolls back whole, leaving nothing to clean up.
//
// A caller naming no image at all is refused: it is outside the protocol the
// handshake belongs to. Where staging is allowed, every other handshake is
// answered with errStagePull before the store is even consulted — a pull is
// staged whether or not the store already holds its image, so the runtime's
// own content store keeps the layers of everything it pulled. Otherwise the
// store decides: an image it does not hold, or holds without the requested
// chain (the shape a moved tag takes while the caller unpacks the manifest
// the tag now points at), is declined. Every check runs before the
// transaction, so no answer leaves a record behind.
//
// Admitting the image's last chain also pins the image under this
// snapshotter. From then on containerd holds a durable record standing for
// content it will only mount later, and the hold is what stops a knarr remove
// from taking that content away in between — a pull followed by a remove
// followed by a run would otherwise fail at the run. The hold is taken before
// the record exists, because a record with no hold would leave content
// containerd counts on reclaimable, and dropped again if the record then fails
// to land.
func (s *snapshotter) createAndCommitIntermediateVirtualSnapshot(ctx context.Context, key, parent, name string, labels map[string]string, opts []snapshots.Opt) error {
	imageRef := labels[imageRefLabel]
	if imageRef == "" {
		return fmt.Errorf("layer chain %q was pulled without image-identity annotations; containerd forwards them only when its config sets enable_remote_snapshot_annotations = \"true\" under [proxy_plugins.cfs.exports]: %w", name, errdefs.ErrFailedPrecondition)
	}
	if s.allowUnstagedPulls {
		return errStagePull
	}
	info, err := s.engine.Inspect(ctx, imageRef)
	if errors.Is(err, knarr.ErrNotFound) {
		return fmt.Errorf("image %q is not present in the local store: %w", imageRef, errdefs.ErrFailedPrecondition)
	}
	if err != nil {
		return fmt.Errorf("inspect image %q for layer chain %q: %w", imageRef, name, err)
	}
	found := false
	for _, layer := range info.Layers {
		if layer.ChainID.String() == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("layer chain %q does not belong to image %q: %w", name, imageRef, errdefs.ErrFailedPrecondition)
	}

	private := map[string]string{chainMemberLabel: "true"}
	if n := len(info.Layers); info.Layers[n-1].ChainID.String() == name {
		holder := recordHolder(name)
		if err := s.engine.Hold(ctx, info.Root.String(), holder); err != nil {
			return fmt.Errorf("hold image %q: %w", imageRef, err)
		}
		private[rootDigestLabel] = info.Root.String()
		private[holderLabel] = holder
	}

	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		if _, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent, opts...); err != nil {
			return fmt.Errorf("create intermediate snapshot %q: %w", name, err)
		}
		chainLabels := []snapshots.Opt{
			snapshots.WithLabels(labels),
			snapshots.WithLabels(private),
		}
		if _, err := storage.CommitActive(ctx, key, name, snapshots.Usage{}, append(opts, chainLabels...)...); err != nil {
			return fmt.Errorf("commit intermediate snapshot %q: %w", name, err)
		}
		return nil
	})
	if err != nil && !errdefs.IsAlreadyExists(err) {
		// The record this hold belongs to was never written, so nothing will
		// ever release it. An already-existing record is not such a case: the
		// holder is named after the record, so the hold this call took is the
		// one that record already stands for.
		if holder := private[holderLabel]; holder != "" {
			s.releaseHold(ctx, info.Root.String(), holder)
		}
		return err
	}
	log.G(ctx).WithFields(log.Fields{
		"key":    key,
		"name":   name,
		"parent": parent,
	}).Info("committed intermediate virtual snapshot")
	return fmt.Errorf("intermediate virtual snapshot %q: %w", name, errdefs.ErrAlreadyExists)
}

// commitRecord commits the active snapshot under key as name, recording the
// disk usage of its directory; a staging record has none and costs nothing.
// private names this snapshotter's own labels the committed record is to
// carry; nil carries over whatever the active record already had.
func (s *snapshotter) commitRecord(ctx context.Context, name, key string, private map[string]string, opts []snapshots.Opt) error {
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, info, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		var usage snapshots.Usage
		if info.Labels[stagingLabel] == "" {
			du, err := fs.DiskUsage(ctx, s.getSnapshotDir(id))
			if err != nil {
				return err
			}
			usage = snapshots.Usage(du)
		}
		own := private
		if own == nil {
			own = privateLabels(info.Labels)
		}
		if len(own) > 0 {
			opts = append(opts, snapshots.WithLabels(own))
		}
		if _, err := storage.CommitActive(ctx, key, name, usage, opts...); err != nil {
			return fmt.Errorf("failed to commit snapshot: %w", err)
		}
		return nil
	})
}

// createStagingSnapshot records the active snapshot of a staged layer and
// returns its placeholder mount. The record is all there is to it: the layer
// goes into the knarr store through the differ, so no directory is
// provisioned and no ancestor is consulted.
func (s *snapshotter) createStagingSnapshot(ctx context.Context, key, parent string, opts []snapshots.Opt) ([]mount.Mount, error) {
	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		if _, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent, opts...); err != nil {
			return fmt.Errorf("failed to create staging snapshot: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.G(ctx).WithFields(log.Fields{"key": key, "parent": parent}).Debug("cfs snapshotter created staging snapshot")
	return stagingMount(key), nil
}

// createSnapshot records an active or view snapshot in the metadata store
// and provisions its overlay directories within the same transaction: a
// failed provisioning rolls the record back, and a failed transaction commit
// leaves no directory behind. The record — key, parent chain, kind, labels —
// is what a snapshot durably is; the directories and the mounts over them
// are the data plane.
func (s *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) ([]mount.Mount, error) {
	var (
		sn   storage.Snapshot
		path string
	)
	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		var terr error
		if sn, terr = storage.CreateSnapshot(ctx, kind, key, parent, opts...); terr != nil {
			return fmt.Errorf("failed to create snapshot: %w", terr)
		}
		path = s.getSnapshotDir(sn.ID)
		return provisionOverlayDirs(path, kind)
	})
	if err != nil {
		// The rollback discards the record; the directory, if it was
		// created before the transaction failed to commit, is ours to drop.
		if path != "" {
			if rerr := os.RemoveAll(path); rerr != nil {
				log.G(ctx).WithError(rerr).WithField("path", path).Warn("failed to remove snapshot directory after failed create")
			}
		}
		return nil, err
	}

	log.G(ctx).WithFields(log.Fields{
		"key":    key,
		"parent": parent,
		"kind":   kind,
	}).Debug("cfs snapshotter created snapshot")

	mounts, err := s.mounts(ctx, key)
	if err != nil {
		if removeErr := s.Remove(ctx, key); removeErr != nil && !errdefs.IsNotFound(removeErr) {
			log.G(ctx).WithError(removeErr).WithField("key", key).Warn("failed to roll back snapshot after mount failure")
		}
		return nil, err
	}
	return mounts, nil
}

// provisionOverlayDirs creates a snapshot directory and, for an active
// snapshot, the upper and work directories its overlay mount names inside it.
// The upper is where the snapshot's own writes land — docker's init files, a
// running container's changes — and where a committed snapshot's content lives
// when a descendant stacks it as a lower.
func provisionOverlayDirs(dir string, kind snapshots.Kind) error {
	if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("failed to create snapshot dir: %w", err)
	}
	if kind != snapshots.KindActive {
		return nil
	}
	for _, name := range []string{"upper", "work"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0755); err != nil { //nolint:gosec // G301: the merged root inherits the upper's mode and must be world-traversable
			return fmt.Errorf("failed to create %s dir: %w", name, err)
		}
	}
	return nil
}

// getSnapshotDir returns the directory backing the snapshot with the given id.
func (s *snapshotter) getSnapshotDir(id string) string {
	return filepath.Join(s.root, "snapshots", id)
}

// mounts returns the mounts exposing the filesystem of the snapshot identified
// by key: one flat overlay over the lowers composed from its parent chain and,
// for an active snapshot, its upper and work directories. A staging snapshot
// has nothing to mount and gets its placeholder instead. No write transaction
// may be opened while mountMu is held: Remove holds a write transaction while
// waiting for that mutex, and bbolt serializes writers.
func (s *snapshotter) mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	// Resolve the snapshot and its full ancestor list, nearest parent first,
	// in one read transaction for a consistent view of the chain. The list
	// mixes real deltas and image chain snapshots, and each ancestor's labels
	// come along because they are what tells the two apart.
	var (
		id        string
		kind      snapshots.Kind
		staging   bool
		ancestors []ancestor
	)
	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		var (
			info snapshots.Info
			terr error
		)
		if id, info, _, terr = storage.GetInfo(ctx, key); terr != nil {
			return fmt.Errorf("failed to get snapshot info: %w", terr)
		}
		if info.Kind != snapshots.KindActive && info.Kind != snapshots.KindView {
			return fmt.Errorf("requested snapshot %s has kind %s, want active or view: %w", key, info.Kind, errdefs.ErrFailedPrecondition)
		}
		kind = info.Kind
		if staging = info.Labels[stagingLabel] != ""; staging {
			return nil
		}
		for name := info.Parent; name != ""; {
			aid, ainfo, _, err := storage.GetInfo(ctx, name)
			if err != nil {
				return fmt.Errorf("resolve parent snapshot %s: %w", name, err)
			}
			ancestors = append(ancestors, ancestor{id: aid, name: name, labels: ainfo.Labels})
			name = ainfo.Parent
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if staging {
		return stagingMount(key), nil
	}
	lowers, foundChain, err := s.composeLowers(ctx, ancestors)
	if err != nil {
		return nil, err
	}
	dir := s.getSnapshotDir(id)
	if !foundChain {
		return nil, fmt.Errorf("snapshot has no image in its ancestry; scratch snapshots are not supported (a pull failing here means containerd sent no unpack handshake: containerd 2.1+ with the cfs unpack_config entry is required): %w", errdefs.ErrInvalidArgument)
	}
	opts := []string{
		"lowerdir=" + strings.Join(lowers, ":") + "::" + s.objectsDir,
		"redirect_dir=on",
		"metacopy=on",
		"verity=require",
	}
	if kind == snapshots.KindActive {
		opts = append([]string{
			"workdir=" + filepath.Join(dir, "work"),
			"upperdir=" + filepath.Join(dir, "upper"),
		}, opts...)
	}
	return []mount.Mount{{Source: "overlay", Type: "overlay", Options: opts}}, nil
}

// ancestor is one entry of a snapshot's parent chain as the mount walk needs
// it: where its directories live, what it is called, and the labels that
// classify it.
type ancestor struct {
	id     string
	name   string
	labels map[string]string
}

// composeLowers walks a snapshot's ancestors, nearest parent first, collecting
// the lower directories its mount stacks, and reports whether the walk reached
// an image.
//
// A real delta contributes its upper directory (provisionOverlayDirs made it)
// and the walk continues. An image chain member ends it: its EROFS metadata
// mount is the last lower, and that mount is the whole image already
// flattened, so older, shorter chain snapshots above it add nothing (and have
// no directory to mount anyway). Stopping is also required for correctness:
// the image has no whiteout markers, so anything stacked below it would leak
// through wherever a layer deleted a file, and shared layer prefixes mean a
// longer image's chain sits above a shorter one's.
//
// The chain member is mounted by the chainID its snapshot.ref label records,
// never by its own name: a chain snapshot committed by the caller is named for
// the caller's own bookkeeping, and only the label names the image content.
func (s *snapshotter) composeLowers(ctx context.Context, ancestors []ancestor) ([]string, bool, error) {
	lowers := make([]string, 0, len(ancestors))
	for _, a := range ancestors {
		dir := s.getSnapshotDir(a.id)
		if !isChainMember(a.labels, dir) {
			lowers = append(lowers, filepath.Join(dir, "upper"))
			continue
		}
		chainID := a.labels[intermediateSnapshotLabel]
		if chainID == "" {
			return nil, false, fmt.Errorf("image snapshot %s names no layer chain in its %s label: %w", a.name, intermediateSnapshotLabel, errdefs.ErrFailedPrecondition)
		}
		fsDir := filepath.Join(dir, "fs")
		if err := s.ensureChainMounted(ctx, chainID, fsDir); err != nil {
			return nil, false, err
		}
		return append(lowers, fsDir), true, nil
	}
	return lowers, false, nil
}

// isChainMember reports whether an ancestor snapshot stands for an image layer
// chain, given its labels and the directory backing it. The chain-member label
// decides it, so a leftover directory cannot mislead the walk, and a staging
// record — metadata only, like a chain member, but standing for a layer whose
// image is not finished — never is one. A record committed before those
// labels existed has none to read and falls back to how it looks on disk: an
// image chain snapshot is metadata only, while every delta owns the upper and
// work directories its content lives in.
func isChainMember(labels map[string]string, dir string) bool {
	if labels[chainMemberLabel] != "" {
		return true
	}
	if labels[stagingLabel] != "" {
		return false
	}
	return !pathExists(filepath.Join(dir, "upper")) && !pathExists(filepath.Join(dir, "work"))
}

// ensureChainMounted mounts an image's read-only EROFS metadata at fsDir if the
// shared mount is not already present. The knarr store resolves the layer chain
// to a stored image and mounts it in one call.
func (s *snapshotter) ensureChainMounted(ctx context.Context, chainID, fsDir string) error {
	s.mountMu.Lock()
	defer s.mountMu.Unlock()

	mounted, err := s.isMounted(fsDir)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	if err := os.MkdirAll(fsDir, 0755); err != nil { //nolint:gosec // G301: rootfs mount target must be world-traversable
		return fmt.Errorf("create chain mount dir %s: %w", fsDir, err)
	}
	if err := s.engine.MountChain(ctx, chainID, fsDir, knarr.WithMetadataOnly()); err != nil {
		if errors.Is(err, knarr.ErrNotFound) {
			return fmt.Errorf("image not found in the knarr store; pull it with knarr before running the container (chain %s): %w", chainID, errdefs.ErrFailedPrecondition)
		}
		return fmt.Errorf("mount image chain %s at %s: %w", chainID, fsDir, err)
	}
	return nil
}

// unmountDirIfMounted tears down a shared image metadata mount in dir.
// Snapshots with no such mount are left untouched.
func (s *snapshotter) unmountDirIfMounted(ctx context.Context, dir string) error {
	s.mountMu.Lock()
	defer s.mountMu.Unlock()

	fsDir := filepath.Join(dir, "fs")
	mounted, err := s.isMounted(fsDir)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	if err := s.engine.Unmount(ctx, fsDir); err != nil {
		return fmt.Errorf("unmount %s: %w", fsDir, err)
	}
	return nil
}

// pathExists reports whether the path itself exists, without following a
// final symlink.
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// isMounted reports whether path is a mount point, treating a missing path as
// not mounted.
func (s *snapshotter) isMounted(path string) (bool, error) {
	mounted, err := s.mounted(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("check mount %s: %w", path, err)
	}
	return mounted, nil
}
