// Copyright 2018 The gVisor Authors.
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

// Package mm provides a memory management subsystem. See README.md for a
// detailed overview.
//
// Lock order:
//
//	fs locks, except for memmap.Mappable locks
//		mm.MemoryManager.metadataMu
//			mm.MemoryManager.mappingMu
//				Locks taken by memmap.MappingIdentity and memmap.Mappable methods other
//				than Translate
//					kernel.TaskSet.mu
//						mm.MemoryManager.activeMu
//							Locks taken by memmap.Mappable.Translate
//								platform.AddressSpace locks
//									memmap.File locks
//					mm.aioManager.mu
//						mm.AIOContext.mu
//
// Only mm.MemoryManager.Fork is permitted to lock mm.MemoryManager.activeMu in
// multiple mm.MemoryManagers, as it does so in a well-defined order (forked
// child first).
package mm

import (
	"sync/atomic"

	"github.com/maxnasonov/gvisor/pkg/abi/linux"
	"github.com/maxnasonov/gvisor/pkg/atomicbitops"
	"github.com/maxnasonov/gvisor/pkg/hostarch"
	"github.com/maxnasonov/gvisor/pkg/safemem"
	"github.com/maxnasonov/gvisor/pkg/sentry/arch"
	"github.com/maxnasonov/gvisor/pkg/sentry/memmap"
	"github.com/maxnasonov/gvisor/pkg/sentry/pgalloc"
	"github.com/maxnasonov/gvisor/pkg/sentry/platform"
	"github.com/maxnasonov/gvisor/pkg/sentry/vfs"
)

// MapsCallbackFunc has all the parameters required for populating an entry of /proc/[pid]/maps.
type MapsCallbackFunc func(start, end hostarch.Addr, permissions hostarch.AccessType, private string, offset uint64, devMajor, devMinor uint32, inode uint64, path string)

// MemoryManager implements a virtual address space.
//
// +stateify savable
type MemoryManager struct {
	// p and mfp are immutable.
	p platform.Platform

	// mf is the cached result of mfp.MemoryFile().
	//
	// mf is immutable.
	mf *pgalloc.MemoryFile `state:"nosave"`

	// haveASIO is the cached result of p.SupportsAddressSpaceIO(). Aside from
	// eliminating an indirect call in the hot I/O path, this makes
	// MemoryManager.asioEnabled() a leaf function, allowing it to be inlined.
	//
	// haveASIO is immutable.
	haveASIO bool `state:"nosave"`

	// layout is the memory layout.
	//
	// layout is set by the binary loader before the MemoryManager can be used.
	layout arch.MmapLayout

	// users is the number of dependencies on the mappings in the MemoryManager.
	// When the number of references in users reaches zero, all mappings are
	// unmapped.
	users atomicbitops.Int32

	// mappingMu is analogous to Linux's struct mm_struct::mmap_sem.
	mappingMu mappingRWMutex `state:"nosave"`

	// vmas stores virtual memory areas. Since vmas are stored by value,
	// clients should usually use vmaIterator.ValuePtr() instead of
	// vmaIterator.Value() to get a pointer to the vma rather than a copy.
	//
	// Invariants: vmas are always page-aligned.
	//
	// vmas is protected by mappingMu.
	vmas vmaSet

	// brk is the mm's brk, which is manipulated using the brk(2) system call.
	// The brk is initially set up by the loader which maps an executable
	// binary into the mm.
	//
	// brk is protected by mappingMu.
	brk hostarch.AddrRange

	// usageAS is vmas.Span(), cached to accelerate RLIMIT_AS checks.
	//
	// usageAS is protected by mappingMu.
	usageAS uint64

	// lockedAS is the combined size in bytes of all vmas with vma.mlockMode !=
	// memmap.MLockNone.
	//
	// lockedAS is protected by mappingMu.
	lockedAS uint64

	// dataAS is the size of private data segments, like mm_struct->data_vm.
	// It means the vma which is private, writable, not stack.
	//
	// dataAS is protected by mappingMu.
	dataAS uint64

	// New VMAs created by MMap use whichever of memmap.MMapOpts.MLockMode or
	// defMLockMode is greater.
	//
	// defMLockMode is protected by mappingMu.
	defMLockMode memmap.MLockMode

	// activeMu is loosely analogous to Linux's struct
	// mm_struct::page_table_lock.
	activeMu activeRWMutex `state:"nosave"`

	// pmas stores platform mapping areas used to implement vmas. Since pmas
	// are stored by value, clients should usually use pmaIterator.ValuePtr()
	// instead of pmaIterator.Value() to get a pointer to the pma rather than
	// a copy.
	//
	// Inserting or removing segments from pmas should happen along with a
	// call to mm.insertRSS or mm.removeRSS.
	//
	// Invariants: pmas are always page-aligned. If a pma exists for a given
	// address, a vma must also exist for that address.
	//
	// pmas is protected by activeMu.
	pmas pmaSet

	// curRSS is pmas.Span(), cached to accelerate updates to maxRSS. It is
	// reported as the MemoryManager's RSS.
	//
	// maxRSS should be modified only via insertRSS and removeRSS, not
	// directly.
	//
	// maxRSS is protected by activeMu.
	curRSS uint64

	// maxRSS is the maximum resident set size in bytes of a MemoryManager.
	// It is tracked as the application adds and removes mappings to pmas.
	//
	// maxRSS should be modified only via insertRSS, not directly.
	//
	// maxRSS is protected by activeMu.
	maxRSS uint64

	// as is the platform.AddressSpace that pmas are mapped into. active is the
	// number of contexts that require as to be non-nil; if active == 0, as may
	// be nil.
	//
	// as is protected by activeMu. active is manipulated with atomic memory
	// operations; transitions to and from zero are additionally protected by
	// activeMu. (This is because such transitions may need to be atomic with
	// changes to as.)
	as     platform.AddressSpace `state:"nosave"`
	active atomicbitops.Int32    `state:"zerovalue"`

	// unmapAllOnActivate indicates that the next Activate call should activate
	// an empty AddressSpace.
	//
	// This is used to ensure that an AddressSpace cached in
	// NewAddressSpace is not used after some change in the MemoryManager
	// or VMAs has made that AddressSpace stale.
	//
	// unmapAllOnActivate is protected by activeMu. It must only be set when
	// there is no active or cached AddressSpace. If as != nil, then
	// invalidations should be propagated immediately.
	unmapAllOnActivate bool `state:"nosave"`

	// If captureInvalidations is true, calls to MM.Invalidate() are recorded
	// in capturedInvalidations rather than being applied immediately to pmas.
	// This is to avoid a race condition in MM.Fork(); see that function for
	// details.
	//
	// Both captureInvalidations and capturedInvalidations are protected by
	// activeMu. Neither need to be saved since captureInvalidations is only
	// enabled during MM.Fork(), during which saving can't occur.
	captureInvalidations  bool             `state:"zerovalue"`
	capturedInvalidations []invalidateArgs `state:"nosave"`

	// dumpability describes if and how this MemoryManager may be dumped to
	// userspace. This is read under kernel.TaskSet.mu, so it can't be protected
	// by metadataMu.
	dumpability atomicbitops.Int32

	metadataMu metadataMutex `state:"nosave"`

	// argv is the application argv. This is set up by the loader and may be
	// modified by prctl(PR_SET_MM_ARG_START/PR_SET_MM_ARG_END). No
	// requirements apply to argv; we do not require that argv.WellFormed().
	//
	// argv is protected by metadataMu.
	argv hostarch.AddrRange

	// envv is the application envv. This is set up by the loader and may be
	// modified by prctl(PR_SET_MM_ENV_START/PR_SET_MM_ENV_END). No
	// requirements apply to envv; we do not require that envv.WellFormed().
	//
	// envv is protected by metadataMu.
	envv hostarch.AddrRange

	// auxv is the ELF's auxiliary vector.
	//
	// auxv is protected by metadataMu.
	auxv arch.Auxv

	// executable is the executable for this MemoryManager. If executable
	// is not nil, it holds a reference on the Dirent.
	//
	// executable is protected by metadataMu.
	executable *vfs.FileDescription

	// aioManager keeps track of AIOContexts used for async IOs. AIOManager
	// must be cloned when CLONE_VM is used.
	aioManager aioManager

	// sleepForActivation indicates whether the task should report to be sleeping
	// before trying to activate the address space. When set to true, delays in
	// activation are not reported as stuck tasks by the watchdog.
	sleepForActivation bool

	// vdsoSigReturnAddr is the address of 'vdso_sigreturn'.
	vdsoSigReturnAddr uint64

	// membarrierPrivateEnabled is non-zero if EnableMembarrierPrivate has
	// previously been called. Since, as of this writing,
	// MEMBARRIER_CMD_PRIVATE_EXPEDITED is implemented as a global memory
	// barrier, membarrierPrivateEnabled has no other effect.
	membarrierPrivateEnabled atomicbitops.Uint32

	// membarrierRSeqEnabled is non-zero if EnableMembarrierRSeq has previously
	// been called.
	membarrierRSeqEnabled atomicbitops.Uint32
}

// vma represents a virtual memory area.
//
// Note: new fields added to this struct must be added to vma.Copy and
// vmaSetFunctions.Merge.
//
// +stateify savable
type vma struct {
	// mappable is the virtual memory object mapped by this vma. If mappable is
	// nil, the vma represents an anonymous mapping.
	mappable memmap.Mappable

	// off is the offset into mappable at which this vma begins. If mappable is
	// nil, off is meaningless.
	off uint64

	// To speedup VMA save/restore, we group and save the following booleans
	// as a single integer.

	// realPerms are the memory permissions on this vma, as defined by the
	// application.
	realPerms hostarch.AccessType `state:".(int)"`

	// effectivePerms are the memory permissions on this vma which are
	// actually used to control access.
	//
	// Invariant: effectivePerms == realPerms.Effective().
	effectivePerms hostarch.AccessType `state:"manual"`

	// maxPerms limits the set of permissions that may ever apply to this
	// memory, as well as accesses for which usermem.IOOpts.IgnorePermissions
	// is true (e.g. ptrace(PTRACE_POKEDATA)).
	//
	// Invariant: maxPerms == maxPerms.Effective().
	maxPerms hostarch.AccessType `state:"manual"`

	// private is true if this is a MAP_PRIVATE mapping, such that writes to
	// the mapping are propagated to a copy.
	private bool `state:"manual"`

	// growsDown is true if the mapping may be automatically extended downward
	// under certain conditions. If growsDown is true, mappable must be nil.
	//
	// There is currently no corresponding growsUp flag; in Linux, the only
	// architectures that can have VM_GROWSUP mappings are ia64, parisc, and
	// metag, none of which we currently support.
	growsDown bool `state:"manual"`

	// dontfork is the MADV_DONTFORK setting for this vma configured by madvise().
	dontfork bool

	mlockMode memmap.MLockMode

	// numaPolicy is the NUMA policy for this vma set by mbind().
	numaPolicy linux.NumaPolicy

	// numaNodemask is the NUMA nodemask for this vma set by mbind().
	numaNodemask uint64

	// If id is not nil, it controls the lifecycle of mappable and provides vma
	// metadata shown in /proc/[pid]/maps, and the vma holds a reference.
	id memmap.MappingIdentity

	// If hint is non-empty, it is a description of the vma printed in
	// /proc/[pid]/maps. hint takes priority over id.MappedName().
	hint string

	// lastFault records the last address that was paged faulted. It hints at
	// which direction addresses in this vma are being accessed.
	//
	// This field can be read atomically, and written with mm.activeMu locked for
	// writing and mm.mapping locked.
	lastFault uintptr
}

func (v *vma) copy() vma {
	return vma{
		mappable:       v.mappable,
		off:            v.off,
		realPerms:      v.realPerms,
		effectivePerms: v.effectivePerms,
		maxPerms:       v.maxPerms,
		private:        v.private,
		growsDown:      v.growsDown,
		dontfork:       v.dontfork,
		mlockMode:      v.mlockMode,
		numaPolicy:     v.numaPolicy,
		numaNodemask:   v.numaNodemask,
		id:             v.id,
		hint:           v.hint,
		lastFault:      atomic.LoadUintptr(&v.lastFault),
	}
}

// pma represents a platform mapping area.
//
// +stateify savable
type pma struct {
	// file is the file mapped by this pma. Only pmas for which file is of type
	// pgalloc.MemoryFile may be saved. pmas hold a reference to the
	// corresponding file range while they exist.
	file memmap.File `state:".(string)"`

	// off is the offset into file at which this pma begins.
	off uint64

	// translatePerms is the permissions returned by memmap.Mappable.Translate.
	// If private is true, translatePerms is hostarch.AnyAccess.
	translatePerms hostarch.AccessType

	// effectivePerms is the permissions allowed for non-ignorePermissions
	// accesses. maxPerms is the permissions allowed for ignorePermissions
	// accesses. These are vma.effectivePerms and vma.maxPerms respectively,
	// masked by pma.translatePerms and with Write disallowed if pma.needCOW is
	// true.
	//
	// These are stored in the pma so that the IO implementation can avoid
	// iterating mm.vmas when pmas already exist.
	effectivePerms hostarch.AccessType
	maxPerms       hostarch.AccessType

	// needCOW is true if writes to the mapping must be propagated to a copy.
	needCOW bool

	// private is true if this pma represents private memory.
	//
	// If private is true, file must be MemoryManager.mfp.MemoryFile(), and
	// calls to Invalidate for which memmap.InvalidateOpts.InvalidatePrivate is
	// false should ignore the pma.
	//
	// If private is false, this pma caches a translation from the
	// corresponding vma's memmap.Mappable.Translate.
	private bool

	// If internalMappings is not empty, it is the cached return value of
	// file.MapInternal for the memmap.FileRange mapped by this pma.
	internalMappings safemem.BlockSeq `state:"nosave"`
}

type invalidateArgs struct {
	ar   hostarch.AddrRange
	opts memmap.InvalidateOpts
}
