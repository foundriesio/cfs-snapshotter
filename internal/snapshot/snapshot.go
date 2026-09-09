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
// answered as if the content were already present locally, or it is staged
// into the snapshot directories as the runtime fetches it and built into the
// store when the chain's last layer is committed (see the calling contract
// below).
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
//	  ... the caller fetches and applies the layer tar into the mounts ...
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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/foundriesio/knarr/pkg/knarr"
	"github.com/moby/sys/mountinfo"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

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
// stagingLabel marks a snapshot created to receive the layer content of a
// staged pull. Staged content is unverified scratch, so such a snapshot's
// mounts stack no image beneath it and require no fs-verity.
//
// rootDigestLabel records the root digest a snapshot's image is held under, so
// the hold is released against the digest that was taken rather than against a
// reference a tag may have moved in the meantime.
//
// holderLabel records the holder name the record took that hold under, so a
// record releases its own hold and not another record's.
//
// The last four are private to this snapshotter. containerd hands a backend
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
	return fmt.Sprintf("cfs-%x", sha256.Sum256([]byte(name)))
}

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
}

// Compile-time assertion that *snapshotter satisfies the containerd interface.
var _ snapshots.Snapshotter = (*snapshotter)(nil)

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
}

// NewSnapshotter returns a snapshots.Snapshotter configured by cfg, serving
// composefs images from the knarr store at cfg.KnarrRoot.
func NewSnapshotter(ctx context.Context, cfg Config) (snapshots.Snapshotter, error) {
	engine, err := knarr.New(knarr.Config{Root: cfg.KnarrRoot})
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
	return &snapshotter{
		root:               root,
		objectsDir:         objectsDir,
		ms:                 ms,
		engine:             engine,
		blobs:              newContentBlobs(cfg.ContainerdAddress),
		mounted:            mountinfo.Mounted,
		allowUnstagedPulls: cfg.AllowUnstagedPulls,
	}, nil
}

// engineAPI is the subset of knarr.Engine the snapshotter uses: reading the
// local store (Inspect, ObjectsDir), mounting an image chain from it
// (MountChain, Unmount), building an image out of layers the runtime already
// fetched (Pull), and pinning what it hands containerd for as long as
// containerd references it (Hold, Release). Building happens at Commit, once a
// chain is whole; a Prepare still never reaches for content over the network.
// Publish and Remove are deliberately excluded — the snapshotter publishes
// nothing and deletes nobody's image — so snapshotter code cannot casually
// reach for them. The narrow surface is also the seam the tests fake.
type engineAPI interface {
	Inspect(ctx context.Context, ref string) (*knarr.ImageInfo, error)
	Pull(ctx context.Context, ref string, opts ...knarr.PullOption) (*knarr.Image, error)
	Hold(ctx context.Context, ref, holder string) error
	Release(ctx context.Context, ref, holder string) error
	MountChain(ctx context.Context, chainID, target string, opts ...knarr.MountOption) error
	Unmount(ctx context.Context, target string) error
	ObjectsDir() (string, error)
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
// an image costs that image's composefs blob, and a staged record whose
// directories were discarded once its chain was built costs nothing. Only a
// record that is neither — a container's own layer — reports the usage
// recorded at commit time. After an unpack the caller queries every member of
// the image's chain in turn to price the image, so a Usage(chainID)+Stat(chainID)
// pair per layer is the normal post-pull pattern.
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
// handshake instead gets a directory-backed staging snapshot to receive the
// layer, whether or not the store holds its image. Everything else
// gets a directory-backed active snapshot and overlay mounts composing the
// image metadata, the knarr objects, the uppers of its committed ancestors,
// and its own upper (see mounts).
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
		// plain writable, still empty snapshot — the handshake's "not
		// present" answer: the caller then fetches the layer, applies it
		// into the snapshot, and commits it itself. It is marked as
		// staging, which is what keeps its mounts from claiming an image
		// beneath content nothing has verified.
		return s.createSnapshot(ctx, snapshots.KindActive, key, parent,
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
// Committing a staged snapshot is also where staged layers become an image.
// The chain being committed is read from the record's own snapshot.ref label,
// never from the name: containerd hands the backend names of its own making,
// and only the label says which layer chain the record stands for. Everything
// but the chain's last layer is filed as it stands; the last one is where the
// whole chain is built into the knarr store out of the layers the runtime
// fetched, pinned there for as long as containerd references it, and recorded
// as an image chain member — from which instant a mount over it composes the
// image and not the staged directories, whether or not those have been swept
// yet.
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

	root, err := s.buildStagedChain(ctx, labels)
	if err != nil {
		return err
	}
	if root == "" {
		return s.commitRecord(ctx, name, key, nil, opts)
	}
	holder := recordHolder(name)
	if err := s.engine.Hold(ctx, root, holder); err != nil {
		return fmt.Errorf("hold image %q: %w", labels[imageRefLabel], err)
	}
	private := map[string]string{chainMemberLabel: "true", rootDigestLabel: root, holderLabel: holder}
	if err := s.commitRecord(ctx, name, key, private, opts); err != nil {
		// No record carries this holder, so nothing would ever release it. A
		// record that already exists is not such a case: the holder is named
		// after the record, so the hold this call took is the one that record
		// already stands for.
		if !errdefs.IsAlreadyExists(err) {
			s.releaseHold(ctx, root, holder)
		}
		return err
	}
	s.sweepStagedChain(ctx, name)
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
// let go once containerd has forgotten the last of them.
func (s *snapshotter) Remove(ctx context.Context, key string) (err error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: remove")
	var (
		renamed, path, root, holder string
		restore                     bool
	)
	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, info, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot info: %w", err)
		}
		// A record that took a hold carries both labels, written together.
		root = info.Labels[rootDigestLabel]
		holder = info.Labels[holderLabel]
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
	if root != "" {
		s.releaseHold(ctx, root, holder)
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

// Close releases the snapshotter's metadata store and the containerd
// connection it reads image blobs over. It is safe to call on an
// already-closed snapshotter.
func (s *snapshotter) Close() error {
	log.L.Trace("cfs snapshotter: close")
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
// disk usage of its directory. private names this snapshotter's own labels the
// committed record is to carry; nil carries over whatever the active record
// already had.
func (s *snapshotter) commitRecord(ctx context.Context, name, key string, private map[string]string, opts []snapshots.Opt) error {
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, info, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		usage, err := fs.DiskUsage(ctx, s.getSnapshotDir(id))
		if err != nil {
			return err
		}
		own := private
		if own == nil {
			own = privateLabels(info.Labels)
		}
		if len(own) > 0 {
			opts = append(opts, snapshots.WithLabels(own))
		}
		if _, err := storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
			return fmt.Errorf("failed to commit snapshot: %w", err)
		}
		return nil
	})
}

// buildStagedChain builds the composefs image of a staged layer chain out of
// the blobs the runtime fetched. It returns the root digest the image was built
// under, which the caller pins and records, or an empty digest when the chain
// being committed is not the image's last one — an image is worth building
// once, whole, and the earlier chains of the same pull are only steps towards
// it.
//
// The labels the runtime attached are what the build is anchored on: the
// reference names the image, the manifest digest fixes which image that is, and
// the chain says where in it this record sits. A build always runs; the knarr
// store recognizing content it already has, verified, is what makes a repeat
// cheap, and no cheaper shortcut can vouch for a store that has been damaged
// since.
func (s *snapshotter) buildStagedChain(ctx context.Context, labels map[string]string) (string, error) {
	chainID := labels[intermediateSnapshotLabel]
	if chainID == "" {
		return "", fmt.Errorf("staged snapshot names no layer chain in its %s label: %w", intermediateSnapshotLabel, errdefs.ErrFailedPrecondition)
	}
	imageRef := labels[imageRefLabel]
	if imageRef == "" {
		return "", fmt.Errorf("staged layer chain %q names no image in its %s label: %w", chainID, imageRefLabel, errdefs.ErrFailedPrecondition)
	}
	root := labels[manifestDigestLabel]
	if root == "" {
		return "", fmt.Errorf("staged layer chain %q names no manifest in its %s label: %w", chainID, manifestDigestLabel, errdefs.ErrFailedPrecondition)
	}

	leaf, err := s.leafChainID(ctx, digest.Digest(root))
	if err != nil {
		return "", err
	}
	if leaf.String() != chainID {
		return "", nil
	}
	if _, err := s.engine.Pull(ctx, imageRef, knarr.WithBlobSource(s.blobs), knarr.WithResolvedRoot(digest.Digest(root))); err != nil {
		return "", fmt.Errorf("build image %q from the staged layers of chain %s: %w", imageRef, chainID, err)
	}
	return root, nil
}

// leafChainID returns the cumulative chain identity of the last layer of the
// manifest at root, which is the identity of the whole image.
func (s *snapshotter) leafChainID(ctx context.Context, root digest.Digest) (digest.Digest, error) {
	var manifest ocispec.Manifest
	if err := s.decodeBlob(ctx, root, &manifest); err != nil {
		return "", fmt.Errorf("read manifest %s: %w", root, err)
	}
	var config ocispec.Image
	if err := s.decodeBlob(ctx, manifest.Config.Digest, &config); err != nil {
		return "", fmt.Errorf("read image config %s: %w", manifest.Config.Digest, err)
	}
	if len(config.RootFS.DiffIDs) == 0 {
		return "", fmt.Errorf("image config %s declares no layers: %w", manifest.Config.Digest, errdefs.ErrFailedPrecondition)
	}
	return identity.ChainID(config.RootFS.DiffIDs), nil
}

// decodeBlob reads the JSON document dgst names out of containerd's content
// store into v.
func (s *snapshotter) decodeBlob(ctx context.Context, dgst digest.Digest, v any) error {
	r, err := s.blobs.Open(ctx, ocispec.Descriptor{Digest: dgst})
	if err != nil {
		return err
	}
	defer func() {
		if err := r.Close(); err != nil {
			log.G(ctx).WithError(err).WithField("digest", dgst).Warn("failed to close content store blob")
		}
	}()
	return json.NewDecoder(r).Decode(v)
}

// sweepStagedChain discards the overlay directories of the staged chain that
// was built into the image now committed as name. Their content lives in the
// knarr store from here on, and every mount over the chain composes the image
// instead, so the directories are duplicate bytes. Failing to discard them
// costs disk, never correctness, which is why it is reported and not returned.
func (s *snapshotter) sweepStagedChain(ctx context.Context, name string) {
	dirs, err := s.stagedChainDirs(ctx, name)
	if err != nil {
		log.G(ctx).WithError(err).WithField("name", name).Warn("failed to resolve the staged chain to discard")
		return
	}
	for _, dir := range dirs {
		for _, sub := range []string{"upper", "work"} {
			path := filepath.Join(dir, sub)
			if err := os.RemoveAll(path); err != nil {
				log.G(ctx).WithError(err).WithField("path", path).Warn("failed to discard staged directory")
			}
		}
	}
}

// stagedChainDirs returns the directories of the record committed as name and
// of the staged records below it, nearest first. The walk stops at the first
// record another snapshot outside the chain descends from: a concurrent pull of
// an image sharing this prefix may still be applying layers over that record's
// upper, and that pull discards what it staged when its own records are
// removed.
func (s *snapshotter) stagedChainDirs(ctx context.Context, name string) ([]string, error) {
	var dirs []string
	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		children := map[string]int{}
		if err := storage.WalkInfo(ctx, func(_ context.Context, info snapshots.Info) error {
			if info.Parent != "" {
				children[info.Parent]++
			}
			return nil
		}); err != nil {
			return fmt.Errorf("count snapshot children: %w", err)
		}
		// The committed record is expected to have no children at all; each
		// record below it has the one the walk arrived from.
		key, inChain := name, 0
		for key != "" {
			id, info, _, err := storage.GetInfo(ctx, key)
			if err != nil {
				return fmt.Errorf("resolve staged snapshot %s: %w", key, err)
			}
			if key != name && info.Labels[stagingLabel] == "" {
				return nil
			}
			if children[key] > inChain {
				return nil
			}
			dirs = append(dirs, s.getSnapshotDir(id))
			key, inChain = info.Parent, 1
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dirs, nil
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
// for an active snapshot, its upper and work directories. A staging snapshot,
// which has no image to descend from, gets a plain overlay of the staged
// uppers instead (see stagingMounts). No write transaction may be opened while
// mountMu is held: Remove holds a write transaction while waiting for that
// mutex, and bbolt serializes writers.
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
		staging = info.Labels[stagingLabel] != ""
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
	lowers, foundChain, err := s.composeLowers(ctx, ancestors)
	if err != nil {
		return nil, err
	}
	dir := s.getSnapshotDir(id)
	if !foundChain {
		if !staging {
			return nil, fmt.Errorf("snapshot has no image in its ancestry; scratch snapshots are not supported (a pull failing here means containerd sent no unpack handshake: containerd 2.1+ with the cfs unpack_config entry is required): %w", errdefs.ErrInvalidArgument)
		}
		return stagingMounts(dir, lowers), nil
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
// decides it, so a leftover directory cannot mislead the walk. A record
// committed before that label existed has none to read and falls back to how
// it looks on disk: an image chain snapshot is metadata only, while every
// delta owns the upper and work directories its content lives in.
func isChainMember(labels map[string]string, dir string) bool {
	if labels[chainMemberLabel] != "" {
		return true
	}
	return !pathExists(filepath.Join(dir, "upper")) && !pathExists(filepath.Join(dir, "work"))
}

// stagingMounts returns the mounts for a staging snapshot in dir over the
// staged uppers below it: a plain overlay, or a bind of its own upper when
// nothing is below it. Staged layers are scratch nothing has verified yet, so
// the mount stacks neither an image nor the knarr objects under them and asks
// the kernel for no fs-verity.
func stagingMounts(dir string, lowers []string) []mount.Mount {
	upper := filepath.Join(dir, "upper")
	if len(lowers) == 0 {
		return []mount.Mount{{Source: upper, Type: "bind", Options: []string{"rw", "rbind"}}}
	}
	return []mount.Mount{{Source: "overlay", Type: "overlay", Options: []string{
		"workdir=" + filepath.Join(dir, "work"),
		"upperdir=" + upper,
		"lowerdir=" + strings.Join(lowers, ":"),
	}}}
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
