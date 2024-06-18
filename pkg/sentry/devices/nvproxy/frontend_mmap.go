// Copyright 2023 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nvproxy

import (
	"github.com/maxnasonov/gvisor/pkg/context"
	"github.com/maxnasonov/gvisor/pkg/errors/linuxerr"
	"github.com/maxnasonov/gvisor/pkg/hostarch"
	"github.com/maxnasonov/gvisor/pkg/log"
	"github.com/maxnasonov/gvisor/pkg/safemem"
	"github.com/maxnasonov/gvisor/pkg/sentry/memmap"
	"github.com/maxnasonov/gvisor/pkg/sentry/vfs"
)

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (fd *frontendFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	return vfs.GenericConfigureMMap(&fd.vfsfd, fd, opts)
}

// AddMapping implements memmap.Mappable.AddMapping.
func (fd *frontendFD) AddMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) error {
	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
func (fd *frontendFD) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
}

// CopyMapping implements memmap.Mappable.CopyMapping.
func (fd *frontendFD) CopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	return nil
}

// Translate implements memmap.Mappable.Translate.
func (fd *frontendFD) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	return []memmap.Translation{
		{
			Source: optional,
			File:   &fd.memmapFile,
			Offset: optional.Start,
			Perms:  at,
		},
	}, nil
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable.
func (fd *frontendFD) InvalidateUnsavable(ctx context.Context) error {
	return nil
}

// +stateify savable
type frontendFDMemmapFile struct {
	memmap.NoBufferedIOFallback

	fd *frontendFD
}

// IncRef implements memmap.File.IncRef.
func (mf *frontendFDMemmapFile) IncRef(fr memmap.FileRange, memCgID uint32) {
}

// DecRef implements memmap.File.DecRef.
func (mf *frontendFDMemmapFile) DecRef(fr memmap.FileRange) {
}

// MapInternal implements memmap.File.MapInternal.
func (mf *frontendFDMemmapFile) MapInternal(fr memmap.FileRange, at hostarch.AccessType) (safemem.BlockSeq, error) {
	// FIXME(jamieliu): determine if this is safe
	log.Traceback("nvproxy: rejecting frontendFDMemmapFile.MapInternal")
	return safemem.BlockSeq{}, linuxerr.EINVAL
}

// FD implements memmap.File.FD.
func (mf *frontendFDMemmapFile) FD() int {
	return int(mf.fd.hostFD)
}
