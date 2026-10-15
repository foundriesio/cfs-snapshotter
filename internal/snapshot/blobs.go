// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	contentapi "github.com/containerd/containerd/api/services/content/v1"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/dialer"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// contentBlobs reads image blobs out of containerd's content store, which is
// where the layers of a staged pull land as the runtime fetches them. It is
// the source the ocicfs store builds an image from, in place of a registry.
//
// The connection is dialed on the first read and shared from then on, so a
// snapshotter that never converts anything never opens it, and a dial that
// fails leaves nothing cached for the next attempt to trip over.
type contentBlobs struct {
	// address is containerd's gRPC socket.
	address string
	// mu guards the lazily dialed connection and the client over it.
	mu     sync.Mutex
	conn   *grpc.ClientConn
	client contentapi.ContentClient
}

// Compile-time assertion that *contentBlobs is a ocicfs blob source.
var _ ocicfs.BlobSource = (*contentBlobs)(nil)

// newContentBlobs returns a blob source reading containerd's content store at
// the given gRPC address.
func newContentBlobs(address string) *contentBlobs {
	return &contentBlobs{address: address}
}

// Open returns the bytes the descriptor names. The read runs in the namespace
// of the request being served — a docker pull's blobs live in "moby" and a
// CRI pull's in "k8s.io", and the content store shows nothing at all to a call
// that names neither. The caller closes the returned reader.
//
// The whole blob comes over one streaming Read call: asked for no size, the
// content service sends it from its first byte to its last, in messages of
// its own choosing, so the blob's length is never needed — a descriptor
// reconstructed from a label carries none, and the stream ends where the
// blob does. The service looks the blob up only once the call is under way,
// so the first message is received here: a blob the store does not hold is
// reported by Open, as a build scanning ahead of the pull expects, and not by
// the first read.
func (b *contentBlobs) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve the namespace to read blob %s in: %w", desc.Digest, err)
	}
	client, err := b.contentClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(namespaces.WithNamespace(ctx, ns))
	stream, err := client.Read(ctx, &contentapi.ReadContentRequest{Digest: desc.Digest.String()})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open blob %s in namespace %s: %w", desc.Digest, ns, errgrpc.ToNative(err))
	}
	r := &blobReader{stream: stream, cancel: cancel}
	if err := r.next(); err != nil && !errors.Is(err, io.EOF) {
		cancel()
		return nil, fmt.Errorf("open blob %s in namespace %s: %w", desc.Digest, ns, err)
	}
	return r, nil
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
	b.conn, b.client = nil, nil
	return conn.Close()
}

// contentClient returns containerd's content service client, dialing on
// first use.
func (b *contentBlobs) contentClient() (contentapi.ContentClient, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.client != nil {
		return b.client, nil
	}
	if b.address == "" {
		return nil, fmt.Errorf("no containerd address to read staged layers from: %w", errdefs.ErrInvalidArgument)
	}
	// The message-size caps match what containerd grants its own clients.
	conn, err := grpc.NewClient(dialer.DialAddress(b.address),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer.ContextDialer),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize),
			grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize)))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd at %q: %w", b.address, err)
	}
	b.conn, b.client = conn, contentapi.NewContentClient(conn)
	return b.client, nil
}

// blobReader reads one blob off the stream of a content Read call. Close
// cancels the call, which releases the stream whether or not it was drained.
type blobReader struct {
	stream contentapi.Content_ReadClient
	cancel context.CancelFunc
	// data is the unread remainder of the last message received.
	data []byte
	// err is what ended the stream, io.EOF once the blob is exhausted; it
	// is returned again by every read that follows.
	err error
}

func (r *blobReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		if err := r.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// next receives messages until one carries data, or the stream ends.
func (r *blobReader) next() error {
	for r.err == nil && len(r.data) == 0 {
		resp, err := r.stream.Recv()
		switch {
		case err == nil:
			r.data = resp.Data
		case errors.Is(err, io.EOF):
			r.err = io.EOF
		default:
			r.err = errgrpc.ToNative(err)
		}
	}
	return r.err
}

func (r *blobReader) Close() error {
	r.cancel()
	return nil
}
