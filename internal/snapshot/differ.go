// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/foundriesio/knarr/pkg/knarr"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// stagingMountType is the type of the one mount a staging snapshot returns.
// No filesystem of that name exists, so anything that tries to mount it —
// containerd's own differ, handed the snapshot because the cfs differ is not
// wired in — fails with ENODEV before a byte of the layer is written. The
// mount's only consumer is Apply below, which reads the snapshot key from
// Source.
const stagingMountType = "cfs-staging"

// stagingMount returns the placeholder mount of the staging snapshot under key.
func stagingMount(key string) []mount.Mount {
	return []mount.Mount{{Type: stagingMountType, Source: key}}
}

// Compile-time assertion that *snapshotter is the differ the daemon serves.
var _ diff.Applier = (*snapshotter)(nil)

// Apply feeds one layer of a staged pull into the knarr store. The unpacker
// calls it between Prepare and Commit of each layer, with the layer's
// descriptor and the mounts Prepare returned; here those mounts are the
// staging placeholder, whose source names the staging snapshot. Its labels
// say which image the pull is anchored on and which layer chain this record
// stands for, and the manifest read out of the content store says where in
// the image that layer sits — the descriptor must be that layer's, so a mount
// left over from another pull can never label the wrong blob.
//
// The layer is ingested by the image's build, started here on its first
// layer and looked up for the rest; the build reads the layer back out of
// containerd's content store, where the fetch has just put it. Ingest blocks
// until the layer is in the store and returns its verification failure, so
// a layer containerd goes on to commit is one the store holds and verified.
// The last layer also finishes the build, which holds the image under the
// build's holder, and is marked as such on its record first — along with the
// image's identity and that holder — so the Commit that follows, inside
// containerd's transaction, files the record without touching the store.
// Mounts of any other shape belong to another snapshotter's pull and are
// declined, so a diff service that tries this differ first falls through to
// the next.
func (s *snapshotter) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount, _ ...diff.ApplyOpt) (ocispec.Descriptor, error) {
	if len(mounts) != 1 || mounts[0].Type != stagingMountType {
		return ocispec.Descriptor{}, fmt.Errorf("cfs differ applies staged layers only: %w", errdefs.ErrNotImplemented)
	}
	key := mounts[0].Source
	log.G(ctx).WithFields(log.Fields{"key": key, "digest": desc.Digest}).Trace("cfs differ: apply")
	start := time.Now()
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("apply staged layer to %q: %w", key, err)
	}
	labels, err := s.stagingLabels(ctx, key)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	layer, err := s.resolveStagedLayer(ctx, labels)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := layer.bind(desc); err != nil {
		return ocispec.Descriptor{}, err
	}
	b, err := s.buildFor(ctx, ns, key, layer)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if layer.leaf {
		// buildFor moved the last layer's build to finishing under this key;
		// it leaves when this apply is over, or when a Remove of the record
		// takes and closes it meanwhile — in which case it is not here to
		// take, and nothing more is owed.
		defer s.takeFinishing(key)
		if err := s.markLeaf(ctx, key, layer.root, b.holder); err != nil {
			closeBuilds(s.takeFinishing(key))
			return ocispec.Descriptor{}, err
		}
	}
	if err := s.ingest(ctx, ns, b, layer); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("build image %q from the staged layers of chain %s: %w", layer.imageRef, layer.chainID, err)
	}
	log.G(ctx).WithFields(log.Fields{
		"key":     key,
		"chain":   layer.chainID,
		"layer":   layer.index,
		"leaf":    layer.leaf,
		"elapsed": time.Since(start).Seconds(),
	}).Info("cfs differ: applied staged layer")
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: layer.diffID}, nil
}

// stagingLabels returns the labels of the staging snapshot under key. The
// record is what makes a placeholder mount trustworthy: only an active
// snapshot this snapshotter marked as staging may have its layer applied.
func (s *snapshotter) stagingLabels(ctx context.Context, key string) (map[string]string, error) {
	var info snapshots.Info
	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		var err error
		_, info, _, err = storage.GetInfo(ctx, key)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("resolve staging snapshot %q: %w", key, err)
	}
	if info.Kind != snapshots.KindActive || info.Labels[stagingLabel] == "" {
		return nil, fmt.Errorf("snapshot %q is not an active staging snapshot: %w", key, errdefs.ErrFailedPrecondition)
	}
	return info.Labels, nil
}

// markLeaf records on the staging snapshot under key that its layer is the
// image's last, which image that is, and the holder the build finishing it
// holds the image under. The markers are written before the layer is
// ingested, so they are on the record whenever a Commit arrives — after a
// daemon restart included — and the record releases the holder when it is
// removed uncommitted, as an unwound pull's is.
func (s *snapshotter) markLeaf(ctx context.Context, key string, root digest.Digest, holder string) error {
	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		mark := snapshots.Info{Name: key, Labels: map[string]string{
			leafLabel:       "true",
			rootDigestLabel: root.String(),
			holderLabel:     holder,
		}}
		_, err := storage.UpdateInfo(ctx, mark, "labels."+leafLabel, "labels."+rootDigestLabel, "labels."+holderLabel)
		return err
	})
	if err != nil {
		return fmt.Errorf("mark staging snapshot %q as the image's last layer: %w", key, err)
	}
	return nil
}

// stagedLayer is where a staged record's layer sits in the image its pull is
// anchored on.
type stagedLayer struct {
	// imageRef and root are the reference and manifest digest the runtime
	// stamped the pull with; chainID is the layer chain the record stands for.
	imageRef string
	root     digest.Digest
	chainID  string
	// index is the layer's position in the manifest, desc its layer
	// descriptor there, and diffID the digest of its uncompressed content.
	index  int
	desc   ocispec.Descriptor
	diffID digest.Digest
	// leaf reports the image's last layer; leafChain is that layer's chain
	// identity, which is the identity of the whole image.
	leaf      bool
	leafChain string
}

// resolveStagedLayer reads the manifest and config a staged record's labels
// name out of the content store and locates the record's layer chain in the
// image. The labels are what the build is anchored on: the reference names
// the image, the manifest digest fixes which image that is, and the chain says
// where in it this record sits; a chain the manifest does not contain is
// refused.
func (s *snapshotter) resolveStagedLayer(ctx context.Context, labels map[string]string) (stagedLayer, error) {
	layer := stagedLayer{
		imageRef: labels[imageRefLabel],
		root:     digest.Digest(labels[manifestDigestLabel]),
		chainID:  labels[intermediateSnapshotLabel],
	}
	if layer.chainID == "" {
		return stagedLayer{}, fmt.Errorf("staged snapshot names no layer chain in its %s label: %w", intermediateSnapshotLabel, errdefs.ErrFailedPrecondition)
	}
	if layer.imageRef == "" {
		return stagedLayer{}, fmt.Errorf("staged layer chain %q names no image in its %s label: %w", layer.chainID, imageRefLabel, errdefs.ErrFailedPrecondition)
	}
	if layer.root == "" {
		return stagedLayer{}, fmt.Errorf("staged layer chain %q names no manifest in its %s label: %w", layer.chainID, manifestDigestLabel, errdefs.ErrFailedPrecondition)
	}

	var manifest ocispec.Manifest
	if err := s.decodeBlob(ctx, layer.root, &manifest); err != nil {
		return stagedLayer{}, fmt.Errorf("read manifest %s: %w", layer.root, err)
	}
	var config ocispec.Image
	if err := s.decodeBlob(ctx, manifest.Config.Digest, &config); err != nil {
		return stagedLayer{}, fmt.Errorf("read image config %s: %w", manifest.Config.Digest, err)
	}
	diffIDs := config.RootFS.DiffIDs
	if len(diffIDs) == 0 || len(diffIDs) != len(manifest.Layers) {
		return stagedLayer{}, fmt.Errorf("image config %s declares %d layers, manifest %s %d: %w", manifest.Config.Digest, len(diffIDs), layer.root, len(manifest.Layers), errdefs.ErrFailedPrecondition)
	}
	// ChainIDs rewrites its argument in place.
	chains := identity.ChainIDs(slices.Clone(diffIDs))
	i := slices.IndexFunc(chains, func(c digest.Digest) bool { return c.String() == layer.chainID })
	if i < 0 {
		return stagedLayer{}, fmt.Errorf("layer chain %q does not belong to image %q: %w", layer.chainID, layer.imageRef, errdefs.ErrFailedPrecondition)
	}
	layer.index = i
	layer.desc = manifest.Layers[i]
	layer.diffID = diffIDs[i]
	layer.leaf = i == len(chains)-1
	layer.leafChain = chains[len(chains)-1].String()
	return layer, nil
}

// bind checks that the descriptor a differ was asked to apply is the
// manifest's descriptor of this layer. The digest must match; the media type
// and size are compared when the caller supplied them.
func (l stagedLayer) bind(desc ocispec.Descriptor) error {
	if desc.Digest != l.desc.Digest {
		return fmt.Errorf("layer %s is not layer %d of image %q, which is %s: %w", desc.Digest, l.index, l.imageRef, l.desc.Digest, errdefs.ErrFailedPrecondition)
	}
	if desc.MediaType != "" && desc.MediaType != l.desc.MediaType {
		return fmt.Errorf("layer %s has media type %s, image %q declares %s: %w", desc.Digest, desc.MediaType, l.imageRef, l.desc.MediaType, errdefs.ErrFailedPrecondition)
	}
	if desc.Size != 0 && desc.Size != l.desc.Size {
		return fmt.Errorf("layer %s has size %d, image %q declares %d: %w", desc.Digest, desc.Size, l.imageRef, l.desc.Size, errdefs.ErrFailedPrecondition)
	}
	return nil
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

// buildKey identifies an image build in progress: the namespace whose
// content store its layers are read from, and the manifest digest it is
// built against. Two runtimes pulling the same image into different
// namespaces read different content stores and get separate builds.
type buildKey struct {
	namespace string
	root      string
}

// openBuild is an image build in progress, the chain identity of the
// image's last layer, which is where the build finishes, and the holder the
// finished image is held under.
type openBuild struct {
	build  knarr.Build
	leaf   string
	holder string
}

// buildHolder names the hold a build takes on the image it finishes, after
// the staging snapshot whose layer started the build: containerd files one
// such key per pull, so two pulls of one image — in one namespace or two —
// never share a holder, and removing the record of one leaves the other's
// hold standing. The key carries characters a holder name may not, so it is
// hashed.
func buildHolder(ns, key string) string {
	return fmt.Sprintf("cfs-build-%x", sha256.Sum256([]byte(ns+"/"+key)))
}

// buildFor returns the build of the image a staged layer belongs to, starting
// one when none is open. A pull whose earlier layers went through a previous
// build — resumed after a restart, or after another pull of the same image
// was aborted — gets a new one, which catches up on those layers from the
// content store. The last layer's build moves from the map to finishing, under
// the layer's staging key, in one step: the Finish that spends it happens
// outside buildMu, and a duplicate pull arriving at the leaf meanwhile starts
// a build of its own rather than sharing a spent one, while a Remove of the
// staging record finds the build under its key from this instant on.
func (s *snapshotter) buildFor(ctx context.Context, ns, key string, layer stagedLayer) (*openBuild, error) {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	bk := buildKey{namespace: ns, root: layer.root.String()}
	b, ok := s.builds[bk]
	if !ok {
		holder := buildHolder(ns, key)
		build, err := s.engine.NewBuild(ctx, layer.imageRef,
			knarr.WithBlobSource(s.blobs), knarr.WithResolvedRoot(layer.root), knarr.WithHold(holder))
		if err != nil {
			return nil, fmt.Errorf("begin building image %q from its staged layers: %w", layer.imageRef, err)
		}
		b = &openBuild{build: build, leaf: layer.leafChain, holder: holder}
		s.builds[bk] = b
	}
	if layer.leaf {
		delete(s.builds, bk)
		s.finishing[key] = b
	}
	return b, nil
}

// ingest feeds a layer to its build, blocking until the store holds it, and
// for the image's last layer finishes the build. A failed ingest abandons the
// build: the runtime aborts the pull on the error, and a retried pull starts
// a build of its own. A finished image also closes every other build in the
// namespace that ends in the same chain: two images built from the same
// layers under different manifests share every chain record, so only one of
// the pulls applies the last layer, and the other's build would otherwise
// stay open with nothing left to finish it.
func (s *snapshotter) ingest(ctx context.Context, ns string, b *openBuild, layer stagedLayer) error {
	if !layer.leaf {
		if err := b.build.Ingest(ctx, layer.index); err != nil {
			s.abandonBuild(ctx, buildKey{namespace: ns, root: layer.root.String()}, b)
			return err
		}
		return nil
	}
	if _, err := b.build.Finish(ctx); err != nil {
		return err
	}
	closeBuilds(s.takeBuilds(func(key buildKey, other *openBuild) bool {
		return key.namespace == ns && other.leaf == b.leaf
	}))
	return nil
}

// abandonBuild closes b and drops it from the map, unless another build has
// taken its place there since.
func (s *snapshotter) abandonBuild(ctx context.Context, key buildKey, b *openBuild) {
	s.takeBuilds(func(k buildKey, other *openBuild) bool { return k == key && other == b })
	b.build.Close()
	log.G(ctx).WithFields(log.Fields{"namespace": key.namespace, "root": key.root}).
		Debug("cfs snapshotter: abandoned image build")
}

// closeStagedBuild abandons the build of the pull a staged record belongs
// to, if one is in progress. A staged record still anchored on a manifest is
// one the runtime is unwinding: its abort removes the active snapshot, and
// the garbage collector removes the committed layers below it afterwards.
// Either closes the pull's build, which nothing will finish now. A build
// applying its last layer under the removed record is closed too, which
// waits for a finish already under way: once it returns, the hold that
// finish took, if it did, is there for the caller to release. The
// collector's context names no namespace, so it is read from the key, which
// containerd prefixes with the namespace for every record it files with a
// backend; a key without one is not such a record.
func (s *snapshotter) closeStagedBuild(ctx context.Context, key string, labels map[string]string) {
	if labels[stagingLabel] == "" || labels[manifestDigestLabel] == "" {
		return
	}
	ns, rest, ok := strings.Cut(key, "/")
	if !ok || rest == "" {
		return
	}
	bk := buildKey{namespace: ns, root: labels[manifestDigestLabel]}
	taken := s.takeBuilds(func(k buildKey, _ *openBuild) bool { return k == bk })
	taken = append(taken, s.takeFinishing(key)...)
	if len(taken) == 0 {
		return
	}
	closeBuilds(taken)
	log.G(ctx).WithFields(log.Fields{"namespace": ns, "root": bk.root}).
		Debug("cfs snapshotter: abandoned image build")
}

// takeFinishing removes and returns the build finishing under key, or every
// finishing build when key is empty. Like takeBuilds, it only takes: the
// caller closes what it took after the lock is released.
func (s *snapshotter) takeFinishing(key string) []*openBuild {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	var taken []*openBuild
	for k, b := range s.finishing {
		if key == "" || k == key {
			taken = append(taken, b)
			delete(s.finishing, k)
		}
	}
	return taken
}

// takeBuilds removes the builds keep selects from the map and returns them.
// The map is all buildMu guards: closing a build waits for its scans to
// stop, so a caller closes what it took only after the lock is released, and
// an unwinding pull holds up no other.
func (s *snapshotter) takeBuilds(keep func(buildKey, *openBuild) bool) []*openBuild {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	var taken []*openBuild
	for key, b := range s.builds {
		if keep(key, b) {
			taken = append(taken, b)
			delete(s.builds, key)
		}
	}
	return taken
}

// closeBuilds abandons the given builds.
func closeBuilds(builds []*openBuild) {
	for _, b := range builds {
		b.build.Close()
	}
}
