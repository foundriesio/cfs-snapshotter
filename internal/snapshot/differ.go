// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
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

// Apply feeds one layer of a staged pull into the ocicfs store. The unpacker
// calls it between Prepare and Commit of each layer, with the layer's
// descriptor and the mounts Prepare returned; here those mounts are the
// staging placeholder, whose source names the staging snapshot. Its labels
// say which image the pull is anchored on and which layer chain this record
// stands for, and the manifest read out of the content store says where in
// the image that layer sits — the descriptor must be that layer's, so a mount
// left over from another pull can never label the wrong blob.
//
// A layer whose chain is the whole chain of an image the ocicfs store
// already holds is the same rootfs the store has, and its record is first
// made to stand for that image (see adoptStoredChain), so a base image pulled
// after an image derived from it runs from the store with nothing built.
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
	if !layer.leaf {
		if err := s.adoptStoredChain(ctx, ns, key, layer.chainID); err != nil {
			return ocispec.Descriptor{}, err
		}
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
			if taken := s.takeFinishing(key); len(taken) > 0 {
				closeBuilds(taken)
				s.collectSoon()
			}
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

// adoptStoredChain makes the staging record under key stand for an image
// the ocicfs store already holds whole at chainID, when it holds one: the
// record takes a hold on that image under the build holder of its key and
// carries the leaf markers, so its Commit files a chain member and a
// container over it mounts the stored image. The pull the record belongs to
// goes on ingesting the layer into its own image's build regardless. The
// store is scanned image by image for one whose last chain is chainID; an
// image that cannot be inspected is passed over, as one unreadable binding
// must not hide a healthy match. Candidates are tried smallest root first:
// each is held and then inspected again, since a removal between the scan
// and the hold deletes the binding while the hold still lands on the root
// object, and a record standing for such an image could never mount. A
// candidate that fails that second look, or whose hold finds nothing, is
// let go and the next one tried; with none left the record stays a plain
// staging record. A hold that fails any other way is released as well
// before the failure is reported: its marker may have been written before
// the failure, and no record names it. A build open in the namespace for
// an image ending in this chain is closed once the markers are on the
// record: its pull's last Prepare will meet the committed chain and never
// apply the layer that would finish it.
func (s *snapshotter) adoptStoredChain(ctx context.Context, ns, key, chainID string) error {
	images, err := s.engine.List(ctx)
	if err != nil {
		return fmt.Errorf("list the ocicfs store for layer chain %s: %w", chainID, err)
	}
	var roots []string
	seen := map[string]struct{}{}
	for _, image := range images {
		if _, ok := seen[image.Digest]; ok {
			continue
		}
		seen[image.Digest] = struct{}{}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := s.engine.Inspect(ctx, image.Digest)
		if err != nil {
			log.G(ctx).WithError(err).WithField("digest", image.Digest).
				Debug("cfs differ: skipping a stored image that cannot be inspected")
			continue
		}
		if n := len(info.Layers); n > 0 && info.Layers[n-1].ChainID.String() == chainID {
			roots = append(roots, image.Digest)
		}
	}
	slices.Sort(roots)

	holder := buildHolder(ns, key)
	for _, root := range roots {
		if err := s.engine.Hold(ctx, root, holder); err != nil {
			if errors.Is(err, ocicfs.ErrNotFound) {
				continue
			}
			s.releaseHold(context.WithoutCancel(ctx), root, holder)
			s.collectSoon()
			return fmt.Errorf("hold stored image %s for layer chain %s: %w", root, chainID, err)
		}
		info, err := s.engine.Inspect(ctx, root)
		if err != nil || len(info.Layers) == 0 || info.Layers[len(info.Layers)-1].ChainID.String() != chainID {
			log.G(ctx).WithError(err).WithFields(log.Fields{"digest": root, "chain": chainID}).
				Debug("cfs differ: a stored image went away while being adopted")
			s.releaseHold(context.WithoutCancel(ctx), root, holder)
			s.collectSoon()
			continue
		}
		if err := s.markLeaf(ctx, key, info.Root, holder); err != nil {
			s.releaseHold(context.WithoutCancel(ctx), root, holder)
			s.collectSoon()
			return err
		}
		if taken := s.takeBuilds(func(k buildKey, other *openBuild) bool {
			return k.namespace == ns && other.leaf == chainID
		}); len(taken) > 0 {
			closeBuilds(taken)
			s.collectSoon()
		}
		log.G(ctx).WithFields(log.Fields{"key": key, "chain": chainID, "root": root}).
			Info("cfs differ: adopted stored image for chain")
		return nil
	}
	return nil
}

// markLeaf records on the staging snapshot under key that its layer is an
// image's last — the pull's own, or a stored one adopted at the chain —
// which image that is, and the holder it is held under. The markers are
// written before the layer is ingested, so they are on the record whenever
// a Commit arrives — after a daemon restart included — and the record
// releases the holder when it is removed uncommitted, as an unwound pull's
// is.
func (s *snapshotter) markLeaf(ctx context.Context, key string, root digest.Digest, holder string) error {
	err := s.setLabels(ctx, key, map[string]string{
		leafLabel:       "true",
		rootDigestLabel: root.String(),
		holderLabel:     holder,
	})
	if err != nil {
		return fmt.Errorf("mark staging snapshot %q as the image's last layer: %w", key, err)
	}
	return nil
}

// setLabels writes the given labels on the snapshot record under key in one
// transaction, leaving every other label of the record as it is. An empty
// value removes the label.
func (s *snapshotter) setLabels(ctx context.Context, key string, labels map[string]string) error {
	fieldpaths := make([]string, 0, len(labels))
	for k := range labels {
		fieldpaths = append(fieldpaths, "labels."+k)
	}
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, err := s.updateInfo(ctx, snapshots.Info{Name: key, Labels: labels}, fieldpaths...)
		return err
	})
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

// loadedManifest is one image manifest of the host's platform that the
// runtime's image records name: which manifest, under which names, and the
// layers and layer chains it describes.
type loadedManifest struct {
	root   digest.Digest
	names  []string
	layers []ocispec.Descriptor
	chains []digest.Digest
}

// loadedManifests reads every host-platform image manifest the namespace's
// image records name, out of containerd, ordered by manifest digest. A record
// targets a manifest directly or through an index, whose image manifests are
// taken one level down. A manifest whose config describes no layers, or
// describes another platform, is not an image this host runs, and a record,
// index entry or config the content store does not hold is a platform the
// runtime did not import; both are passed over. The names of every record
// that leads to a manifest are collected on it.
func (s *snapshotter) loadedManifests(ctx context.Context) ([]loadedManifest, error) {
	records, err := s.blobs.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	byRoot := map[digest.Digest]*loadedManifest{}
	for _, record := range records {
		target, err := digest.Parse(record.GetTarget().GetDigest())
		if err != nil {
			return nil, fmt.Errorf("image record %q: %w", record.GetName(), err)
		}
		roots, err := s.manifestsOf(ctx, record.GetTarget().GetMediaType(), target)
		if err != nil {
			return nil, fmt.Errorf("resolve image record %q: %w", record.GetName(), err)
		}
		for _, root := range roots {
			m, seen := byRoot[root]
			if !seen {
				m, err = s.readLoadedManifest(ctx, root)
				if err != nil {
					return nil, fmt.Errorf("read manifest %s of image record %q: %w", root, record.GetName(), err)
				}
				byRoot[root] = m
			}
			if m != nil {
				m.names = append(m.names, record.GetName())
			}
		}
	}
	manifests := make([]loadedManifest, 0, len(byRoot))
	for _, m := range byRoot {
		if m != nil {
			manifests = append(manifests, *m)
		}
	}
	slices.SortFunc(manifests, func(a, b loadedManifest) int { return strings.Compare(a.root.String(), b.root.String()) })
	return manifests, nil
}

// manifestsOf returns the image manifests an image record's target names:
// the target itself when it is a manifest, or the image-manifest entries of
// the index it is. A target of any other kind, or an index the content store
// does not hold, names none.
func (s *snapshotter) manifestsOf(ctx context.Context, mediaType string, target digest.Digest) ([]digest.Digest, error) {
	if images.IsManifestType(mediaType) {
		return []digest.Digest{target}, nil
	}
	if !images.IsIndexType(mediaType) {
		return nil, nil
	}
	var index ocispec.Index
	if err := s.decodeBlob(ctx, target, &index); err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var roots []digest.Digest
	for _, desc := range index.Manifests {
		if images.IsManifestType(desc.MediaType) {
			roots = append(roots, desc.Digest)
		}
	}
	return roots, nil
}

// readLoadedManifest reads the manifest root names and its config out of the
// content store. It returns nil for a manifest or config the store does not
// hold, a config that declares no layers or a layer count the manifest does
// not match, or a platform that is not the host's.
func (s *snapshotter) readLoadedManifest(ctx context.Context, root digest.Digest) (*loadedManifest, error) {
	var manifest ocispec.Manifest
	if err := s.decodeBlob(ctx, root, &manifest); err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var config ocispec.Image
	if err := s.decodeBlob(ctx, manifest.Config.Digest, &config); err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read image config %s: %w", manifest.Config.Digest, err)
	}
	diffIDs := config.RootFS.DiffIDs
	if len(diffIDs) == 0 || len(diffIDs) != len(manifest.Layers) || !hostPlatformConfig(&config) {
		return nil, nil
	}
	// ChainIDs rewrites its argument in place.
	chains := identity.ChainIDs(slices.Clone(diffIDs))
	return &loadedManifest{root: root, layers: manifest.Layers, chains: chains}, nil
}

// hostPlatformConfig reports whether an image config describes the platform
// this snapshotter runs on, which is the only platform the ocicfs store
// builds an image for.
func hostPlatformConfig(config *ocispec.Image) bool {
	return samePlatform(ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}, config.Platform)
}

// samePlatform compares two platforms by OS, architecture and variant, where
// an unstated variant stands for the architecture's default one (v8 on
// arm64, v7 on arm): a config naming the default variant matches a host that
// names none.
func samePlatform(a, b ocispec.Platform) bool {
	if a.OS != b.OS || a.Architecture != b.Architecture {
		return false
	}
	var defaultVariant string
	switch a.Architecture {
	case "arm64":
		defaultVariant = "v8"
	case "arm":
		defaultVariant = "v7"
	}
	return cmp.Or(a.Variant, defaultVariant) == cmp.Or(b.Variant, defaultVariant)
}

// buildableManifest picks the manifest an image whose full layer chain is
// chainID is built from: of the manifests whose last chain it is, the first
// in digest order whose every layer blob the content store holds. The
// digest order makes the choice the same on every call, whatever order the
// records were listed in; a manifest missing a blob could not be built and
// is passed over for one that can.
func (s *snapshotter) buildableManifest(ctx context.Context, manifests []loadedManifest, chainID string) (loadedManifest, bool, error) {
	for _, m := range manifests {
		if m.chains[len(m.chains)-1].String() != chainID {
			continue
		}
		complete := true
		for _, layer := range m.layers {
			present, err := s.blobs.Has(ctx, layer.Digest)
			if err != nil {
				return loadedManifest{}, false, err
			}
			if !present {
				complete = false
				break
			}
		}
		if complete {
			return m, true, nil
		}
	}
	return loadedManifest{}, false, nil
}

// loadedImageRef is the reference an image built from a loaded manifest is
// recorded under in the ocicfs store: the smallest of the names the runtime
// knows it by that is not digest-qualified, or the manifest digest itself
// when every name is — a digest the store lists as it stands, where a
// digest-qualified name would be rewritten into a registry path it never
// came from.
func loadedImageRef(m loadedManifest) string {
	ref := ""
	for _, name := range m.names {
		if strings.Contains(name, "@") {
			continue
		}
		if ref == "" || name < ref {
			ref = name
		}
	}
	if ref == "" {
		return m.root.String()
	}
	return ref
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
	build  ocicfs.Build
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
			ocicfs.WithBlobSource(s.blobs), ocicfs.WithResolvedRoot(layer.root), ocicfs.WithHold(holder))
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
	_, err := b.build.Finish(ctx)
	if err != nil {
		// The build is over with no image to reference what it wrote;
		// its unreferenced objects can be reclaimed.
		s.collectSoon()
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
	s.collectSoon()
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
	s.collectSoon()
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
