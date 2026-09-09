// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	diffapi "github.com/containerd/containerd/api/services/diff/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// diffServer serves containerd's diff API over gRPC from a diff.Applier. It
// applies layers only: a diff request, which would compute a layer out of two
// mount sets, is answered as not implemented, and containerd's diff service
// moves on to the next differ in its order. Apply's options — processor
// payloads and the sync-fs flag — concern extracting a tar onto a mounted
// filesystem, which the applier does not do, so they are not forwarded.
type diffServer struct {
	diffapi.UnimplementedDiffServer
	applier diff.Applier
}

// newDiffServer returns the gRPC diff service serving applier.
func newDiffServer(applier diff.Applier) diffapi.DiffServer {
	return &diffServer{applier: applier}
}

func (s *diffServer) Apply(ctx context.Context, req *diffapi.ApplyRequest) (*diffapi.ApplyResponse, error) {
	desc := ocispec.Descriptor{
		MediaType:   req.Diff.GetMediaType(),
		Digest:      digest.Digest(req.Diff.GetDigest()),
		Size:        req.Diff.GetSize(),
		Annotations: req.Diff.GetAnnotations(),
	}
	applied, err := s.applier.Apply(ctx, desc, mount.FromProto(req.Mounts))
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &diffapi.ApplyResponse{Applied: &types.Descriptor{
		MediaType:   applied.MediaType,
		Digest:      applied.Digest.String(),
		Size:        applied.Size,
		Annotations: applied.Annotations,
	}}, nil
}

func (s *diffServer) Diff(context.Context, *diffapi.DiffRequest) (*diffapi.DiffResponse, error) {
	return nil, errgrpc.ToGRPC(errdefs.ErrNotImplemented)
}
