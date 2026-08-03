// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/content/proxy"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/dialer"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/foundriesio/knarr/pkg/knarr"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// contentBlobs reads image blobs out of containerd's content store, which is
// where the layers of a staged pull land as the runtime fetches them. It is
// the source the knarr store builds an image from, in place of a registry.
//
// The connection is dialed on the first read and shared from then on, so a
// snapshotter that never converts anything never opens it, and a dial that
// fails leaves nothing cached for the next attempt to trip over.
type contentBlobs struct {
	// address is containerd's gRPC socket.
	address string
	// mu guards the lazily dialed connection and the store over it.
	mu    sync.Mutex
	conn  *grpc.ClientConn
	store content.Store
}

// Compile-time assertion that *contentBlobs is a knarr blob source.
var _ knarr.BlobSource = (*contentBlobs)(nil)

// newContentBlobs returns a blob source reading containerd's content store at
// the given gRPC address.
func newContentBlobs(address string) *contentBlobs {
	return &contentBlobs{address: address}
}

// Open returns the bytes the descriptor names. The read runs in the namespace
// of the request being served — a docker pull's blobs live in "moby" and a
// CRI pull's in "k8s.io", and the content store shows nothing at all to a call
// that names neither. The caller closes the returned reader.
func (b *contentBlobs) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve the namespace to read blob %s in: %w", desc.Digest, err)
	}
	store, err := b.contentStore()
	if err != nil {
		return nil, err
	}
	ra, err := store.ReaderAt(namespaces.WithNamespace(ctx, ns), desc)
	if err != nil {
		return nil, fmt.Errorf("open blob %s in namespace %s: %w", desc.Digest, ns, err)
	}
	// The store knows how long the blob really is; a descriptor reconstructed
	// from a label or a manifest may not carry a size at all.
	return blobReader{Reader: io.NewSectionReader(ra, 0, ra.Size()), closer: ra}, nil
}

// Close drops the containerd connection. A second call is a no-op, so tearing
// the snapshotter down twice does not report the connection as already closed.
func (b *contentBlobs) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	conn := b.conn
	if conn == nil {
		return nil
	}
	b.conn, b.store = nil, nil
	return conn.Close()
}

// contentStore returns containerd's content store, dialing on first use.
func (b *contentBlobs) contentStore() (content.Store, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.store != nil {
		return b.store, nil
	}
	if b.address == "" {
		return nil, fmt.Errorf("no containerd address to read staged layers from: %w", errdefs.ErrInvalidArgument)
	}
	// The message-size caps match what containerd grants its own clients; a
	// content Info response carries every label of the blob, which can
	// outgrow gRPC's stock limit.
	conn, err := grpc.NewClient(dialer.DialAddress(b.address),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer.ContextDialer),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize),
			grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize)))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd at %q: %w", b.address, err)
	}
	b.conn, b.store = conn, proxy.NewContentStore(conn)
	return b.store, nil
}

// blobReader streams one blob and releases the store handle it reads through.
type blobReader struct {
	io.Reader
	closer io.Closer
}

func (r blobReader) Close() error {
	return r.closer.Close()
}
