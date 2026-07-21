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
// first image chain snapshot, whose data plane — materialized at first use
// as a stand-in for the image content — is the base lower. The snapshot's
// own upper receives its writes. Content-addressed composefs materialization
// later keeps this exact shape, only putting the real image where the
// stand-in link points. Until then the implementation is a remote-snapshotter
// stub: every unpack handshake is answered as if the content were already
// present locally (see the calling contract below).
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
// This stub always answers that way, so a pull completes with zero layer
// fetches (the caller still downloads the image metadata: index, manifest,
// config, etc.) — but
// the committed records carry no image content. The mounts the
// container flow receives are instead overlays whose base lower is a
// stand-in rootfs (the daemon's -stub-rootfs flag), so the whole lifecycle
// through `docker run` can be exercised, against stand-in content that is
// the same for every image. Materializing real image content behind the
// handshake is what replaces this stub.
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
// materializes only if and when a descendant snapshot needs the image (see
// materializeChain).
const intermediateSnapshotLabel = "containerd.io/snapshot.ref"

// snapshotter is the cfs-snapshotter implementation of snapshots.Snapshotter.
type snapshotter struct {
	// root is the snapshotter's root directory.
	root string
	// ms is the bolt-backed metadata store holding snapshot records.
	ms *storage.MetaStore
	// engine provides access to images in the local knarr store.
	engine engineAPI
	// stubRootfs names the read-only directory standing in for materialized
	// image content: e.g. `docker run --rm <image> /hello` with a static
	// /hello in the directory. Chain snapshots materialize their data plane
	// as a symlink to it — never a copy — so it is the same for every image:
	// it simulates image content, it is not image content.
	stubRootfs string
	// mounted reports whether a path is a mount point; it is mountinfo.Mounted
	// in production and is replaced in tests.
	mounted func(string) (bool, error)
}

// Compile-time assertion that *snapshotter satisfies the containerd interface.
var _ snapshots.Snapshotter = (*snapshotter)(nil)

// NewSnapshotter returns a snapshots.Snapshotter rooted at root, serving the
// stand-in tree at stubRootfs while resolving images from the knarr store at
// knarrRoot.
func NewSnapshotter(ctx context.Context, root, stubRootfs, knarrRoot string) (snapshots.Snapshotter, error) {
	engine, err := knarr.New(knarr.Config{Root: knarrRoot})
	if err != nil {
		return nil, fmt.Errorf("create knarr engine: %w", err)
	}
	return newSnapshotter(ctx, root, stubRootfs, engine)
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

func newSnapshotter(ctx context.Context, root, stubRootfs string, engine engineAPI) (*snapshotter, error) {
	if root == "" {
		return nil, fmt.Errorf("cfs snapshotter: root directory must not be empty: %w", errdefs.ErrInvalidArgument)
	}
	if strings.ContainsAny(root, ":,") {
		return nil, fmt.Errorf("cfs snapshotter: root directory must not contain ':' or ',' (paths under it become overlay mount options): %w", errdefs.ErrInvalidArgument)
	}
	if fi, err := os.Stat(stubRootfs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("cfs snapshotter: stub rootfs %q is not a directory: %w", stubRootfs, errdefs.ErrInvalidArgument)
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
		ms:         ms,
		engine:     engine,
		stubRootfs: stubRootfs,
		mounted:    mountinfo.Mounted,
	}, nil
}

// engineAPI is the subset of knarr.Engine the snapshotter uses: reading the
// local store (Inspect, ResolveChain, ObjectsDir) and mounting from it (Mount,
// Unmount). Pull, Publish, and Remove are deliberately excluded — content is
// fetched, built, and deleted out of band, never by the snapshotter, which
// only reads and mounts — so snapshotter code cannot casually reach for them.
// The narrow surface is also the seam the tests fake.
type engineAPI interface {
	Inspect(ctx context.Context, ref string) (*knarr.ImageInfo, error)
	ResolveChain(ctx context.Context, chainID string) (*knarr.ImageInfo, error)
	Mount(ctx context.Context, ref, target string, opts ...knarr.MountOption) error
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
func (s *snapshotter) Mounts(ctx context.Context, key string) (_ []mount.Mount, err error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: mounts")
	var sn storage.Snapshot
	err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		sn, err = storage.GetSnapshot(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot mount: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.mounts(ctx, sn)
}

// Prepare records a new active snapshot under key, descending from parent,
// and returns its mounts. Both call shapes from the package contract land
// here, and the snapshot.ref label decides the work: a handshake Prepare
// needs only a metadata record — created and committed virtually in one
// transaction, no directory ever provisioned — and is answered with
// ErrAlreadyExists instead of mounts. Everything else gets a directory-backed
// active snapshot and overlay mounts composing the stand-in rootfs, the
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
		return nil, s.createAndCommitIntermediateVirtualSnapshot(ctx, key, parent, name, base.Labels, opts)
	}

	// Everything else asks for a real writable snapshot whose mounts will
	// actually be executed, for example docker's init layer or the
	// container rootfs.
	return s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

// createAndCommitIntermediateVirtualSnapshot answers the unpack handshake:
// the snapshot the caller asks about is recorded as pure metadata — created
// and committed under the requested name, with the caller's labels and zero
// usage, in one transaction — and ErrAlreadyExists is reported so the caller
// skips fetching and applying the layer. The snapshot is virtual: nothing
// ever mounts an intermediate snapshot, so no directory is provisioned, and
// a concurrent handshake that loses the commit race rolls back whole,
// leaving nothing to clean up. In this stub the record only claims the chain
// exists; the stub's replacement verifies the content really is present
// locally before answering.
func (s *snapshotter) createAndCommitIntermediateVirtualSnapshot(ctx context.Context, key, parent, name string, labels map[string]string, opts []snapshots.Opt) error {
	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
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
	}).Info("stub-committed intermediate virtual snapshot")
	return fmt.Errorf("intermediate virtual snapshot %q: %w", name, errdefs.ErrAlreadyExists)
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
		renamed = filepath.Join(s.root, "snapshots", "rm-"+id)
		if err = os.Rename(path, renamed); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("failed to rename: %w", err)
			}
			renamed = ""
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
		// A directory-less direct parent is an intermediate virtual
		// snapshot: the image chain this snapshot is layered over.
		// Materialize its data plane before finishing the snapshot that
		// needs it — a failure rolls the new record back, and checking only
		// the direct parent suffices because every committed delta's own
		// Prepare already ensured the chain below it. (A parent with a
		// directory needs nothing: it is either a delta or an already
		// materialized chain.)
		if len(sn.ParentIDs) > 0 {
			pdir := s.getSnapshotDir(sn.ParentIDs[0])
			if !pathExists(pdir) {
				if terr := s.materializeChain(ctx, sn.ParentIDs[0]); terr != nil {
					return terr
				}
			}
		}
		path = s.getSnapshotDir(sn.ID)
		return provisionOverlayDirs(path)
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

	return s.mounts(ctx, sn)
}

// provisionOverlayDirs creates the empty upper and work directories the
// snapshot's overlay mounts name inside its directory. The upper is where the
// snapshot's own writes land — docker's init files, a running container's
// changes — and where a committed snapshot's content lives when a descendant
// stacks it as a lower.
func provisionOverlayDirs(dir string) error {
	if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("failed to create snapshot dir: %w", err)
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

// mounts returns the mounts exposing the snapshot's filesystem: one flat
// overlay over the snapshot's own upper and the lowers composed from its
// parent chain.
func (s *snapshotter) mounts(ctx context.Context, sn storage.Snapshot) ([]mount.Mount, error) {
	roFlag := "rw"
	if sn.Kind == snapshots.KindView {
		roFlag = "ro"
	}
	// Walk the ancestors, nearest parent first: a committed delta (it has an
	// upper directory) contributes that upper as a lower and the walk goes
	// on; the first image chain snapshot ends the walk and its data plane —
	// materialized on first use — becomes the base lower. The walk must stop
	// there: a materialized image is a flattened tree carrying no whiteout
	// markers, so anything stacked below it would shine through wherever the
	// image's own layers deleted files (and with shared layer prefixes, a
	// longer image's chain has shorter images' chains beneath it). Every
	// snapshot thus sees an empty upper of its own plus the full composed
	// view below — which is why nothing here ever needs to know whether the
	// caller is preparing docker's init layer or a container rootfs.
	lowers := make([]string, 0, len(sn.ParentIDs)+1)
	for _, pid := range sn.ParentIDs {
		dir := s.getSnapshotDir(pid)
		if fs := filepath.Join(dir, "fs"); pathExists(fs) {
			lowers = append(lowers, fs)
			break
		}
		if upper := filepath.Join(dir, "upper"); pathExists(upper) {
			lowers = append(lowers, upper)
			continue
		}
		// Unreachable when the creation invariant holds: every snapshot's
		// Prepare materialized the chain below it, so an ancestor without a
		// directory means the state was tampered with or an upgrade broke
		// the invariant. Fail rather than compose a wrong rootfs.
		return nil, fmt.Errorf("image chain snapshot %s has no data plane", pid)
	}
	if len(lowers) == 0 {
		// No image in the ancestry: a scratch snapshot. The stub serves
		// only image-backed snapshots, so refuse loudly here rather than
		// emit an overlay spec with no lower that fails at mount time.
		return nil, fmt.Errorf("snapshot has no image in its ancestry; scratch snapshots are not supported: %w", errdefs.ErrInvalidArgument)
	}
	dir := s.getSnapshotDir(sn.ID)
	return []mount.Mount{
		{
			Source: "overlay",
			Type:   "overlay",
			Options: []string{
				roFlag,
				"lowerdir=" + strings.Join(lowers, ":"),
				"upperdir=" + filepath.Join(dir, "upper"),
				"workdir=" + filepath.Join(dir, "work"),
			},
		},
	}, nil
}

// materializeChain gives the chain snapshot with the given id its data plane:
// a directory whose fs entry serves the image content — a symlink to the
// stand-in rootfs (the stub's replacement instead mounts the image's
// composefs metadata here). Materialization is on demand, at the first
// snapshot that needs the image, because the unpack handshake cannot own it:
// handshake Prepares fire only during the pull — every later container
// Prepare stacks directly on the committed chain records — and a real mount,
// unlike this symlink, does not survive a reboot, so the data plane must be
// re-establishable wherever mounts are rebuilt, not built once at pull time.
// Eager materialization would also spend a data plane on every intermediate
// chain and on images that never back a container. Callers run inside the
// metadata store's write transaction, which serializes concurrent prepares
// over the same image.
func (s *snapshotter) materializeChain(ctx context.Context, id string) error {
	dir := s.getSnapshotDir(id)
	fs := filepath.Join(dir, "fs")
	if pathExists(fs) {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create chain snapshot dir: %w", err)
	}
	if err := os.Symlink(s.stubRootfs, fs); err != nil {
		return fmt.Errorf("materialize chain data plane: %w", err)
	}
	log.G(ctx).WithFields(log.Fields{
		"id": id,
		"fs": fs,
	}).Info("materialized stand-in data plane for image chain snapshot")
	return nil
}

// pathExists reports whether the path itself exists, without following a
// final symlink (a chain snapshot's fs is one).
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
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
