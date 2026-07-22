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
// upper receives its writes. The image content is staged out of band in the
// knarr store; the snapshotter only reads and mounts it, and every unpack
// handshake is answered as if the content were already present locally (see
// the calling contract below).
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
const (
	intermediateSnapshotLabel = "containerd.io/snapshot.ref"
	imageRefLabel             = "containerd.io/snapshot/cri.image-ref"
)

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
	// mounted reports whether a path is a mount point; it is mountinfo.Mounted
	// in production and is replaced in tests.
	mounted func(string) (bool, error)
	// mountMu serializes shared chain mount creation and teardown.
	mountMu sync.Mutex
}

// Compile-time assertion that *snapshotter satisfies the containerd interface.
var _ snapshots.Snapshotter = (*snapshotter)(nil)

// NewSnapshotter returns a snapshots.Snapshotter rooted at root, serving
// composefs images from the knarr store at knarrRoot.
func NewSnapshotter(ctx context.Context, root, knarrRoot string) (snapshots.Snapshotter, error) {
	engine, err := knarr.New(knarr.Config{Root: knarrRoot})
	if err != nil {
		return nil, fmt.Errorf("create knarr engine: %w", err)
	}
	return newSnapshotter(ctx, root, engine)
}

func newSnapshotter(ctx context.Context, root string, engine engineAPI) (*snapshotter, error) {
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
		root:       root,
		objectsDir: objectsDir,
		ms:         ms,
		engine:     engine,
		mounted:    mountinfo.Mounted,
	}, nil
}

// engineAPI is the subset of knarr.Engine the snapshotter uses: reading the
// local store (Inspect, ObjectsDir) and mounting an image chain from it
// (MountChain, Unmount). Pull, Publish, and Remove are deliberately excluded —
// content is fetched, built, and deleted out of band, never by the
// snapshotter, which only reads and mounts — so snapshotter code cannot
// casually reach for them. The narrow surface is also the seam the tests fake.
type engineAPI interface {
	Inspect(ctx context.Context, ref string) (*knarr.ImageInfo, error)
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
func (s *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (_ snapshots.Info, err error) {
	log.G(ctx).WithFields(log.Fields{
		"name":       info.Name,
		"kind":       info.Kind,
		"labels":     info.Labels,
		"fieldpaths": fieldpaths,
	}).Trace("cfs snapshotter: update")
	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
		return err
	})
	if err != nil {
		return snapshots.Info{}, err
	}
	return info, nil
}

// Usage reports the disk usage of the snapshot identified by key. For an active
// snapshot it is measured from the snapshot directory; for a committed snapshot
// the usage recorded at commit time is returned. After an unpack the caller
// queries every member of the image's chain in turn to price the image, so a
// Usage(chainID)+Stat(chainID) pair per layer is the normal post-pull pattern.
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
	if info.Kind == snapshots.KindActive {
		du, err := fs.DiskUsage(ctx, s.getSnapshotDir(id))
		if err != nil {
			return snapshots.Usage{}, err
		}
		usage = snapshots.Usage(du)
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
// here, and the snapshot.ref label decides the work: a handshake Prepare
// needs only a metadata record — created and committed virtually in one
// transaction, no directory ever provisioned — and is answered with
// ErrAlreadyExists instead of mounts. Everything else gets a directory-backed
// active snapshot and overlay mounts composing the image metadata, the knarr
// objects, the uppers of its committed ancestors, and its own upper (see
// mounts).
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
		return nil, s.createAndCommitIntermediateVirtualSnapshot(ctx, key, parent, name, base.Labels, opts)
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
// committed snapshot is immutable and exists to be a parent.
func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	log.G(ctx).WithFields(log.Fields{"name": name, "key": key}).
		WithFields(optFields(opts)).Trace("cfs snapshotter: commit")
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, _, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		usage, err := fs.DiskUsage(ctx, s.getSnapshotDir(id))
		if err != nil {
			return err
		}
		if _, err := storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
			return fmt.Errorf("failed to commit snapshot: %w", err)
		}
		return nil
	})
}

// Remove abandons the snapshot identified by key, dropping its metadata record
// and the snapshot directory. The directory is renamed out of the way inside
// the transaction so a failure can restore it. Removal is driven by
// containerd's garbage collector children-first — a container's rootfs
// snapshot goes before its init layer, and image chains unwind leaf-first
// once no image references them.
func (s *snapshotter) Remove(ctx context.Context, key string) (err error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: remove")
	var (
		renamed, path string
		restore       bool
	)
	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
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
	return nil
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

// Close releases the snapshotter's metadata store. It is safe to call on an
// already-closed snapshotter.
func (s *snapshotter) Close() error {
	log.L.Trace("cfs snapshotter: close")
	if s.ms == nil {
		return nil
	}
	return s.ms.Close()
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

// createAndCommitIntermediateVirtualSnapshot answers the unpack handshake only
// when the caller names a locally stored image containing the requested chain.
// The snapshot is recorded as pure metadata — created and committed under the
// requested name, with the caller's labels and zero usage, in one transaction
// — and ErrAlreadyExists is reported so the caller skips fetching and applying
// the layer. Nothing mounts an intermediate snapshot, so no directory is
// provisioned, and a concurrent handshake that loses the commit race rolls
// back whole, leaving nothing to clean up. The admission check runs before the
// transaction, so a decline leaves no record.
func (s *snapshotter) createAndCommitIntermediateVirtualSnapshot(ctx context.Context, key, parent, name string, labels map[string]string, opts []snapshots.Opt) error {
	imageRef := labels[imageRefLabel]
	if imageRef == "" {
		return fmt.Errorf("image reference is required for layer chain %q: %w", name, errdefs.ErrFailedPrecondition)
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

	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		if _, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent, opts...); err != nil {
			return fmt.Errorf("create intermediate snapshot %q: %w", name, err)
		}
		if _, err := storage.CommitActive(ctx, key, name, snapshots.Usage{}, append(opts, snapshots.WithLabels(labels))...); err != nil {
			return fmt.Errorf("commit intermediate snapshot %q: %w", name, err)
		}
		return nil
	})
	if err != nil && !errdefs.IsAlreadyExists(err) {
		return err
	}
	log.G(ctx).WithFields(log.Fields{
		"key":    key,
		"name":   name,
		"parent": parent,
	}).Info("committed intermediate virtual snapshot")
	return fmt.Errorf("intermediate virtual snapshot %q: %w", name, errdefs.ErrAlreadyExists)
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
// for an active snapshot, its upper and work directories. No write transaction
// may be opened while mountMu is held: Remove holds a write transaction while
// waiting for that mutex, and bbolt serializes writers.
func (s *snapshotter) mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	// Resolve the snapshot and its full ancestor list, nearest parent first,
	// in one read transaction for a consistent view of the chain. The list
	// mixes real deltas and virtual chain snapshots; the walk below tells
	// them apart by disk layout, not by anything available here.
	type ancestor struct {
		id   string
		name string
	}
	var (
		id        string
		kind      snapshots.Kind
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
		for name := info.Parent; name != ""; {
			aid, ainfo, _, err := storage.GetInfo(ctx, name)
			if err != nil {
				return fmt.Errorf("resolve parent snapshot %s: %w", name, err)
			}
			ancestors = append(ancestors, ancestor{id: aid, name: name})
			name = ainfo.Parent
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Walk nearest parent first. An ancestor with an upper dir
	// (provisionOverlayDirs made it) is a real delta: stack its upper as a
	// lower and keep going. One with no upper is the image's own chain
	// snapshot — unpack committed it as metadata only — so mount its EROFS
	// metadata and stop: that mount is the whole image already flattened, so
	// older, shorter chain snapshots above it add nothing (and have no
	// directory to mount anyway). Stopping is also required for
	// correctness: the image has no whiteout markers, so anything stacked
	// below it would leak through wherever a layer deleted a file, and
	// shared layer prefixes mean a longer image's chain sits above a
	// shorter one's.
	lowers := make([]string, 0, len(ancestors))
	foundChain := false
	for _, a := range ancestors {
		dir := s.getSnapshotDir(a.id)
		if upper := filepath.Join(dir, "upper"); pathExists(upper) {
			lowers = append(lowers, upper)
			continue
		}
		fsDir := filepath.Join(dir, "fs")
		if err := s.ensureChainMounted(ctx, a.name, fsDir); err != nil {
			return nil, err
		}
		lowers = append(lowers, fsDir)
		foundChain = true
		break // remaining ancestors are older chain snapshots; nothing more to mount
	}
	if !foundChain {
		return nil, fmt.Errorf("snapshot has no image in its ancestry; scratch snapshots are not supported: %w", errdefs.ErrInvalidArgument)
	}
	dir := s.getSnapshotDir(id)
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
