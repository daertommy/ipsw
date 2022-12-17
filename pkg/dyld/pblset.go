package dyld

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unsafe"

	"github.com/blacktop/go-macho/pkg/trie"
	"github.com/blacktop/go-macho/types"
	"github.com/olekukonko/tablewriter"
)

const (
	LoaderMagic            = 0x6c347964 // "l4yd"
	PrebuiltLoaderSetMagic = 0x73703464 // "sp4d"
	NoUnzipperedTwin       = 0xFFFF
)

var ErrPrebuiltLoaderSetNotSupported = fmt.Errorf("dyld_shared_cache has no launch prebuilt loader set info")

type LoaderRef uint16

// index       : 15,   // index into PrebuiltLoaderSet
// app         :  1;   // app vs dyld cache PrebuiltLoaderSet

// Index index into PrebuiltLoaderSet
func (l LoaderRef) Index() uint16 {
	return uint16(types.ExtractBits(uint64(l), 0, 15))
}

// IsApp app vs dyld cache PrebuiltLoaderSet
func (l LoaderRef) IsApp() bool {
	return types.ExtractBits(uint64(l), 15, 1) != 0
}
func (l LoaderRef) IsMissingWeakImage() bool {
	return (l.Index() == 0x7fff) && !l.IsApp()
}
func (l LoaderRef) String() string {
	var typ string
	if l.IsApp() {
		typ = ", type: app"
	}
	mssing_weak_image := ""
	if l.IsMissingWeakImage() {
		mssing_weak_image = " (missing weak image)"
	}
	return fmt.Sprintf("index: %d%s%s", l.Index(), typ, mssing_weak_image)
}

type Loader struct {
	Magic uint32 // "l4yd"
	Info  uint16
	// isPrebuilt         :  1,  // PrebuiltLoader vs JustInTimeLoader
	// dylibInDyldCache   :  1,
	// hasObjC            :  1,
	// mayHavePlusLoad    :  1,
	// hasReadOnlyData    :  1,  // __DATA_CONST.  Don't use directly.  Use hasConstantSegmentsToProtect()
	// neverUnload        :  1,  // part of launch or has non-unloadable data (e.g. objc, tlv)
	// leaveMapped        :  1,  // RTLD_NODELETE
	// hasReadOnlyObjC    :  1,  // Has __DATA_CONST,__objc_selrefs section
	// pre2022Binary      :  1,
	// padding            :  6;
	Ref LoaderRef
}

func (l Loader) IsPrebuilt() bool {
	return types.ExtractBits(uint64(l.Info), 0, 1) != 0
}
func (l Loader) DylibInDyldCache() bool {
	return types.ExtractBits(uint64(l.Info), 1, 1) != 0
}
func (l Loader) HasObjC() bool {
	return types.ExtractBits(uint64(l.Info), 2, 1) != 0
}
func (l Loader) MayHavePlusLoad() bool {
	return types.ExtractBits(uint64(l.Info), 3, 1) != 0
}
func (l Loader) HasReadOnlyData() bool {
	return types.ExtractBits(uint64(l.Info), 4, 1) != 0
}
func (l Loader) NeverUnload() bool {
	return types.ExtractBits(uint64(l.Info), 5, 1) != 0
}
func (l Loader) LeaveMapped() bool {
	return types.ExtractBits(uint64(l.Info), 6, 1) != 0
}
func (l Loader) HasReadOnlyObjC() bool {
	return types.ExtractBits(uint64(l.Info), 7, 1) != 0
}
func (l Loader) Pre2022Binary() bool {
	return types.ExtractBits(uint64(l.Info), 8, 1) != 0
}

func (l Loader) String() string {
	var out []string
	if l.IsPrebuilt() {
		out = append(out, "prebuilt")
	} else {
		out = append(out, "jit")
	}
	if l.DylibInDyldCache() {
		out = append(out, "in-cache-dylib")
	}
	if l.HasObjC() {
		out = append(out, "objc")
	}
	if l.MayHavePlusLoad() {
		out = append(out, "+load")
	}
	if l.HasReadOnlyData() {
		out = append(out, "ro-data")
	}
	if l.NeverUnload() {
		out = append(out, "never-unload")
	}
	if l.LeaveMapped() {
		out = append(out, "leave-mapped")
	}
	if l.HasReadOnlyObjC() {
		out = append(out, "ro-objc")
	}
	if l.Pre2022Binary() {
		out = append(out, "pre-2022")
	}
	return fmt.Sprintf("%s, ref: %s", strings.Join(out, "|"), l.Ref)
}

type DependentKind uint8

const (
	KindNormal   DependentKind = 0
	KindWeakLink DependentKind = 1
	KindReexport DependentKind = 2
	KindUpward   DependentKind = 3
)

func (k DependentKind) String() string {
	switch k {
	case KindNormal:
		return "regular"
	case KindWeakLink:
		return "weak link"
	case KindReexport:
		return "reexport"
	case KindUpward:
		return "upward"
	default:
		return fmt.Sprintf("unknown %d", k)
	}
}

type BindTargetRef uint64

func (b BindTargetRef) LoaderRef() LoaderRef {
	return LoaderRef(types.ExtractBits(uint64(b), 0, 16))
}
func (b BindTargetRef) high8() uint64 {
	return types.ExtractBits(uint64(b), 16, 8)
}
func (b BindTargetRef) low39() uint64 {
	return types.ExtractBits(uint64(b), 24, 39) // signed
}
func (b BindTargetRef) AbsoluteValue() uint64 {
	return deserializeAbsoluteValue(uint64(types.ExtractBits(uint64(b), 0, 63)))
}
func (b BindTargetRef) Kind() uint8 {
	return uint8(types.ExtractBits(uint64(b), 63, 1))
}
func (b BindTargetRef) IsAbsolute() bool {
	return b.Kind() == 1
}
func (b BindTargetRef) Offset() uint64 {
	if b.IsAbsolute() {
		return b.AbsoluteValue()
	}
	signedOffset := b.low39()
	if (signedOffset & 0x0000004000000000) != 0 {
		signedOffset |= 0x00FFFF8000000000
	}
	return (b.high8() << 56) | signedOffset
}
func (b BindTargetRef) String(f *File) string {
	if b.IsAbsolute() {
		return fmt.Sprintf("%#08x: (absolue)", b.Offset())
	}
	if b.LoaderRef().IsApp() {
		return fmt.Sprintf("%#08x: (%s)", b.Offset(), b.LoaderRef())
	}
	return fmt.Sprintf("%#08x: %s", b.Offset(), f.Images[b.LoaderRef().Index()].Name)
}

type CachePatch struct {
	DylibIndex    uint32
	DylibVMOffset uint32
	PatchTo       BindTargetRef
}
type dpkind int64

const (
	endOfPatchTable   dpkind = -1
	missingWeakImport dpkind = 0
	objcClass         dpkind = 1
	singleton         dpkind = 2
)

type DylibPatch struct {
	OverrideOffsetOfImpl int64
	Kind                 dpkind
}

// Region stored in PrebuiltLoaders and generated on the fly by JustInTimeLoaders, passed to mapSegments()
type Region struct {
	Info uint64
	// vmOffset     : 59,
	// perms        :  3,
	// isZeroFill   :  1,
	// readOnlyData :  1;
	FileOffset uint32
	FileSize   uint32 // mach-o files are limited to 4GB, but zero fill data can be very large
}

func (r Region) VMOffset() uint64 {
	return types.ExtractBits(r.Info, 0, 59)
}

func (r Region) Perms() types.VmProtection {
	return types.VmProtection(types.ExtractBits(r.Info, 59, 3))
}

func (r Region) IsZeroFill() bool {
	return types.ExtractBits(r.Info, 62, 1) != 0
}

func (r Region) ReadOnlyData() bool {
	return types.ExtractBits(r.Info, 63, 1) != 0
}

func (r Region) String() string {
	return fmt.Sprintf("file_off: %#x, file_siz: %#x, vm_off: %#x, perms: %s, is_zerofill: %t, ro_data: %t",
		r.FileOffset,
		r.FileSize,
		r.VMOffset(),
		r.Perms(),
		r.IsZeroFill(),
		r.ReadOnlyData())
}

type RSKind uint32

const (
	RSKindRebase RSKind = iota
	RSKindBindToImage
	RSKindBindAbsolute
)

func (k RSKind) String() string {
	switch k {
	case RSKindRebase:
		return "rebase"
	case RSKindBindToImage:
		return "bind to image"
	case RSKindBindAbsolute:
		return "bind absolute"
	default:
		return "unknown"
	}
}

type ResolvedSymbol struct {
	TargetLoader        *Loader
	TargetSymbolName    string
	TargetRuntimeOffset uint64
	Kind                RSKind
	IsCode              bool
	IsWeakDef           bool
	IsMissingFlatLazy   bool
}

type BindTarget struct {
	Loader        *Loader
	RuntimeOffset uint64
}

// fileValidation stored in PrebuiltLoader when it references a file on disk
type fileValidation struct {
	SliceOffset     uint64
	Inode           uint64
	Mtime           uint64
	CDHash          [20]byte // to validate file has not changed since PrebuiltLoader was built
	UUID            types.UUID
	CheckInodeMtime bool
	CheckCDHash     bool
}

type CodeSignatureInFile struct {
	FileOffset uint32
	Size       uint32
}

func deserializeAbsoluteValue(value uint64) uint64 {
	// sign extend
	if (value & 0x4000000000000000) != 0 {
		value |= 0x8000000000000000
	}
	return value
}

type dependent struct {
	Name string
	Kind DependentKind
}

type prebuiltLoaderHeader struct {
	Loader
	PathOffset                     uint16
	DependentLoaderRefsArrayOffset uint16 // offset to array of LoaderRef
	DependentKindArrayOffset       uint16 // zero if all deps normal
	FixupsLoadCommandOffset        uint16

	AltPathOffset        uint16 // if install_name does not match real path
	FileValidationOffset uint16 // zero or offset to FileValidationInfo

	Info uint16
	// hasInitializers      :  1,
	// isOverridable        :  1,      // if in dyld cache, can roots override it
	// supportsCatalyst     :  1,      // if false, this cannot be used in catalyst process
	// isCatalystOverride   :  1,      // catalyst side of unzippered twin
	// regionsCount         : 12
	RegionsOffset uint16 // offset to Region array

	DepCount             uint16
	BindTargetRefsOffset uint16
	BindTargetRefsCount  uint32 // bind targets can be large, so it is last
	// After this point, all offsets in to the PrebuiltLoader need to be 32-bits as the bind targets can be large

	ObjcBinaryInfoOffset uint32 // zero or offset to ObjCBinaryInfo
	IndexOfTwin          uint16 // if in dyld cache and part of unzippered twin, then index of the other twin
	_                    uint16

	ExportsTrieLoaderOffset uint64
	ExportsTrieLoaderSize   uint32
	VmSize                  uint32

	CodeSignature CodeSignatureInFile

	PatchTableOffset uint32

	OverrideBindTargetRefsOffset uint32
	OverrideBindTargetRefsCount  uint32

	// followed by:
	//  path chars
	//  dep kind array
	//  file validation info
	//  segments
	//  bind targets
}

// ObjCBinaryInfo stores information about the layout of the objc sections in a binary,
// as well as other properties relating to the objc information in there.
type ObjCBinaryInfo struct {
	// Offset to the __objc_imageinfo section
	ImageInfoRuntimeOffset uint64

	// Offsets to sections containing objc pointers
	SelRefsRuntimeOffset      uint64
	ClassListRuntimeOffset    uint64
	CategoryListRuntimeOffset uint64
	ProtocolListRuntimeOffset uint64

	// Counts of the above sections.
	SelRefsCount      uint32
	ClassListCount    uint32
	CategoryCount     uint32
	ProtocolListCount uint32

	// Do we have stable Swift fixups to apply to at least one class?
	HasClassStableSwiftFixups bool

	// Do we have any pointer-based method lists to set as uniqued?
	HasClassMethodListsToSetUniqued    bool
	HasCategoryMethodListsToSetUniqued bool
	HasProtocolMethodListsToSetUniqued bool

	// Do we have any method lists in which to set selector references.
	// Note we only support visiting selector refernces in pointer based method lists
	// Relative method lists should have been verified to always point to __objc_selrefs
	HasClassMethodListsToUnique    bool
	HasCategoryMethodListsToUnique bool
	HasProtocolMethodListsToUnique bool
	_                              bool //padding

	// When serialized to the PrebuildLoader, these fields will encode other information about
	// the binary.

	// Offset to an array of uint8_t's.  One for each protocol.
	// Note this can be 0 (ie, have no fixups), even if we have protocols.  That would be the case
	// if this binary contains no canonical protocol definitions, ie, all canonical defs are in other binaries
	// or the shared cache.
	ProtocolFixupsOffset uint32
	// Offset to an array of BindTargetRef's.  One for each selector reference to fix up
	// Note we only fix up selector refs in the __objc_selrefs section, and in pointer-based method lists
	SelectorReferencesFixupsOffset uint32
	SelectorReferencesFixupsCount  uint32
}

func (o ObjCBinaryInfo) String() string {
	var out string
	out += fmt.Sprintf("  __objc_imageinfo: %#08x\n", o.ImageInfoRuntimeOffset)
	out += fmt.Sprintf("  __objc_selrefs:   %#08x (count=%d)\n", o.SelRefsRuntimeOffset, o.SelRefsCount)
	out += fmt.Sprintf("  __objc_classlist: %#08x (count=%d)\n", o.ClassListRuntimeOffset, o.ClassListCount)
	out += fmt.Sprintf("  __objc_catlist:   %#08x (count=%d)\n", o.CategoryListRuntimeOffset, o.CategoryCount)
	out += fmt.Sprintf("  __objc_protolist: %#08x (count=%d)\n", o.ProtocolListRuntimeOffset, o.ProtocolListCount)
	var flags []string
	if o.HasClassStableSwiftFixups {
		flags = append(flags, "class-stable-swift-fixups")
	}
	if o.HasClassMethodListsToSetUniqued {
		flags = append(flags, "class-method-lists-to-set-uniqued")
	}
	if o.HasCategoryMethodListsToSetUniqued {
		flags = append(flags, "category-method-lists-to-set-uniqued")
	}
	if o.HasProtocolMethodListsToSetUniqued {
		flags = append(flags, "protocol-method-lists-to-set-uniqued")
	}
	if o.HasClassMethodListsToUnique {
		flags = append(flags, "class-method-lists-to-unique")
	}
	if o.HasCategoryMethodListsToUnique {
		flags = append(flags, "category-method-lists-to-unique")
	}
	if o.HasProtocolMethodListsToUnique {
		flags = append(flags, "protocol-method-lists-to-unique")
	}
	if len(flags) > 0 {
		out += "\n  flags:\n"
		for _, f := range flags {
			out += fmt.Sprintf("    - %s\n", f)
		}
	}
	return out
}

type PrebuiltLoader struct {
	prebuiltLoaderHeader
	Path                        string
	AltPath                     string
	Twin                        string
	Dependents                  []dependent
	FileValidation              *fileValidation
	Regions                     []Region
	BindTargets                 []BindTargetRef
	DylibPatches                []DylibPatch
	OverrideBindTargets         []BindTargetRef
	ObjcFixupInfo               *ObjCBinaryInfo
	ObjcCanonicalProtocolFixups []bool
	ObjcSelectorFixups          []BindTargetRef
}

func (pl PrebuiltLoader) HasInitializers() bool {
	return types.ExtractBits(uint64(pl.Info), 0, 1) != 0
}
func (pl PrebuiltLoader) IsOverridable() bool {
	return types.ExtractBits(uint64(pl.Info), 1, 1) != 0
}
func (pl PrebuiltLoader) SupportsCatalyst() bool {
	return types.ExtractBits(uint64(pl.Info), 2, 1) != 0
}
func (pl PrebuiltLoader) IsCatalystOverride() bool {
	return types.ExtractBits(uint64(pl.Info), 3, 1) != 0
}
func (pl PrebuiltLoader) RegionsCount() uint16 {
	return uint16(types.ExtractBits(uint64(pl.Info), 4, 12))
}
func (pl PrebuiltLoader) GetInfo() string {
	var out []string
	if pl.HasInitializers() {
		out = append(out, "initializers")
	}
	if pl.IsOverridable() {
		out = append(out, "overridable")
	}
	if pl.SupportsCatalyst() {
		out = append(out, "catalyst")
	}
	if pl.IsCatalystOverride() {
		out = append(out, "catalyst_override")
	}
	return strings.Join(out, "|")
}
func (pl PrebuiltLoader) GetFileOffset(vmoffset uint64) uint64 {
	for _, region := range pl.Regions {
		if vmoffset >= region.VMOffset() && vmoffset < region.VMOffset()+uint64(region.FileSize) {
			return uint64(region.FileOffset) + (vmoffset - region.VMOffset())
		}
	}
	return 0
}
func (pl PrebuiltLoader) String(f *File) string {
	var out string
	if pl.Path != "" {
		out += fmt.Sprintf("Path:    %s\n", pl.Path)
	}
	if pl.AltPath != "" {
		out += fmt.Sprintf("AltPath: %s\n", pl.AltPath)
	}
	if pl.Twin != "" {
		out += fmt.Sprintf("Twin:    %s\n", pl.Twin)
	}
	out += fmt.Sprintf("VM Size:       %#x\n", pl.VmSize)
	if pl.CodeSignature.Size > 0 {
		out += fmt.Sprintf("CodeSignature: off=%#08x, sz=%#x\n", pl.CodeSignature.FileOffset, pl.CodeSignature.Size)
	}
	if pl.FileValidation != nil {
		if pl.FileValidation.CheckCDHash {
			h := sha1.New()
			h.Write(pl.FileValidation.CDHash[:])
			out += fmt.Sprintf("CDHash:        %x\n", h.Sum(nil))
		}
		if pl.FileValidation.CheckInodeMtime {
			out += fmt.Sprintf("slice-offset:  %#x\n", pl.FileValidation.SliceOffset)
			out += fmt.Sprintf("inode          %#x\n", pl.FileValidation.Inode)
			out += fmt.Sprintf("mod-time       %#x\n", pl.FileValidation.Mtime)
		}
		if !pl.FileValidation.UUID.IsNull() {
			out += fmt.Sprintf("UUID:          %s\n", pl.FileValidation.UUID)
		}
	}
	out += fmt.Sprintf("Loader:        %s\n", pl.Loader)
	if len(pl.GetInfo()) > 0 {
		out += fmt.Sprintf("Info:          %s\n", pl.GetInfo())
	}
	if pl.ExportsTrieLoaderSize > 0 {
		out += fmt.Sprintf("ExportsTrie:   off=%#08x, sz=%#x\n", pl.GetFileOffset(pl.ExportsTrieLoaderOffset), pl.ExportsTrieLoaderSize)
	}
	if pl.FixupsLoadCommandOffset > 0 {
		out += fmt.Sprintf("FixupsLoadCmd: off=%#08x\n", pl.FixupsLoadCommandOffset)
	}
	if len(pl.Regions) > 0 {
		out += "\nRegions:\n"
		tableString := &strings.Builder{}
		rdata := [][]string{}
		for _, rg := range pl.Regions {
			rdata = append(rdata, []string{
				fmt.Sprintf("%#08x", rg.FileOffset),
				fmt.Sprintf("%#08x", rg.FileSize),
				fmt.Sprintf("%#08x", rg.VMOffset()),
				rg.Perms().String(),
				fmt.Sprintf("%t", rg.IsZeroFill()),
				fmt.Sprintf("%t", rg.ReadOnlyData()),
			})
		}
		table := tablewriter.NewWriter(tableString)
		table.SetHeader([]string{"File Off", "File Sz", "VM Off", "Perms", "Zero Fill", "RO Data"})
		table.SetBorders(tablewriter.Border{Left: true, Top: false, Right: true, Bottom: false})
		table.SetCenterSeparator("|")
		table.AppendBulk(rdata)
		table.SetAlignment(tablewriter.ALIGN_LEFT)
		table.Render()
		out += tableString.String()
	}
	if len(pl.Dependents) > 0 {
		out += "\nDependents:\n"
		for _, dp := range pl.Dependents {
			out += fmt.Sprintf("\t%-10s) %s\n", dp.Kind, dp.Name)
		}
	}
	if len(pl.BindTargets) > 0 {
		out += "\nBindTargets:\n"
		for _, bt := range pl.BindTargets {
			out += fmt.Sprintf("  %s\n", bt.String(f))
		}
	}
	if len(pl.OverrideBindTargets) > 0 {
		out += "\nOverride BindTargets:\n"
		for _, bt := range pl.OverrideBindTargets {
			out += fmt.Sprintf("  %s\n", bt.String(f))
		}
	}
	if pl.ObjcFixupInfo != nil {
		out += "\nObjC Fixup Info:\n"
		out += fmt.Sprintln(pl.ObjcFixupInfo.String())
	}
	if len(pl.ObjcCanonicalProtocolFixups) > 0 {
		out += "ObjC Canonical ProtocolFixups:\n"
		for _, fixup := range pl.ObjcCanonicalProtocolFixups {
			out += fmt.Sprintf("  %t\n", fixup)
		}
	}
	if len(pl.ObjcSelectorFixups) > 0 {
		out += "\nObjC SelectorFixups:\n"
		for _, bt := range pl.ObjcSelectorFixups {
			out += fmt.Sprintf("  %s\n", bt.String(f))
		}
	}

	return out
}

// PrebuiltLoaderSet is an mmap()ed read-only data structure which holds a set of PrebuiltLoader objects;
// The contained PrebuiltLoader objects can be found be index O(1) or path O(n).
type prebuiltLoaderSetHeader struct {
	Magic                    uint32
	VersionHash              uint32 // PREBUILTLOADER_VERSION
	Length                   uint32
	LoadersArrayCount        uint32
	LoadersArrayOffset       uint32
	CachePatchCount          uint32
	CachePatchOffset         uint32
	DyldCacheUuidOffset      uint32
	MustBeMissingPathsCount  uint32
	MustBeMissingPathsOffset uint32
	// ObjC prebuilt data
	ObjcSelectorHashTableOffset  uint32
	ObjcClassHashTableOffset     uint32
	ObjcProtocolHashTableOffset  uint32
	Reserved                     uint32
	ObjcProtocolClassCacheOffset uint64
	// Swift prebuilt data
	SwiftTypeConformanceTableOffset        uint32
	SwiftMetadataConformanceTableOffset    uint32
	SwiftForeignTypeConformanceTableOffset uint32
}

type PrebuiltLoaderSet struct {
	prebuiltLoaderSetHeader
	Loaders            []PrebuiltLoader
	Patches            []CachePatch
	DyldCacheUUID      types.UUID
	MustBeMissingPaths []string
}

func (pls PrebuiltLoaderSet) HasOptimizedSwift() bool {
	return (pls.SwiftForeignTypeConformanceTableOffset != 0) || (pls.SwiftMetadataConformanceTableOffset != 0) || (pls.SwiftTypeConformanceTableOffset != 0)
}
func (pls PrebuiltLoaderSet) String(f *File) string {
	var out string
	out += "PrebuiltLoaderSet:\n"
	out += fmt.Sprintf("  Version: %x\n", pls.VersionHash)
	if !pls.DyldCacheUUID.IsNull() {
		out += fmt.Sprintf("  DyldCacheUUID: %s\n", pls.DyldCacheUUID)
	}
	if len(pls.Loaders) > 0 {
		out += "\nLoaders:\n"
		for _, pl := range pls.Loaders {
			if len(pls.Loaders) > 1 {
				out += "---\n"
			}
			out += fmt.Sprintln(pl.String(f))
		}
	}
	if len(pls.MustBeMissingPaths) > 0 {
		out += "MustBeMissing:\n"
		for _, path := range pls.MustBeMissingPaths {
			out += fmt.Sprintf("    %s\n", path)
		}
	}
	if len(pls.Patches) > 0 {
		out += "Cache Overrides:\n"
		for _, patch := range pls.Patches {
			if len(pls.Patches) > 1 {
				out += "---\n"
			}
			img := fmt.Sprintf("(index=%d)", patch.DylibIndex)
			if patch.DylibIndex < uint32(len(f.Images)) {
				img = f.Images[patch.DylibIndex].Name
			}
			out += fmt.Sprintf("  cache-dylib:    %s\n", img)
			out += fmt.Sprintf("  dylib-offset:   %#08x\n", patch.DylibVMOffset)
			if patch.PatchTo.LoaderRef().Index() < uint16(len(f.Images)) {
				img = f.Images[patch.PatchTo.LoaderRef().Index()].Name
			} else {
				img = patch.PatchTo.LoaderRef().String()
			}
			out += fmt.Sprintf("  replace-loader: %s\n", img)
			out += fmt.Sprintf("  replace-offset: %#08x\n", patch.PatchTo.Offset())
		}
	}
	return out
}

func (f *File) ForEachLaunchLoaderSet(handler func(execPath string, pset *PrebuiltLoaderSet)) error {
	if f.Headers[f.UUID].MappingOffset < uint32(unsafe.Offsetof(f.Headers[f.UUID].ProgramTrieSize)) {
		return ErrPrebuiltLoaderSetNotSupported
	}
	if f.Headers[f.UUID].ProgramTrieAddr == 0 {
		return ErrPrebuiltLoaderSetNotSupported
	}

	uuid, off, err := f.GetOffset(f.Headers[f.UUID].ProgramTrieAddr)
	if err != nil {
		return err
	}

	dat, err := f.ReadBytesForUUID(uuid, int64(off), uint64(f.Headers[f.UUID].ProgramTrieSize))
	if err != nil {
		return err
	}

	r := bytes.NewReader(dat)

	nodes, err := trie.ParseTrie(r)
	if err != nil {
		return err
	}

	for _, node := range nodes {
		r.Seek(int64(node.Offset), io.SeekStart)

		pblsOff, err := trie.ReadUleb128(r)
		if err != nil {
			return err
		}

		uuid, psetOffset, err := f.GetOffset(f.Headers[f.UUID].ProgramsPblSetPoolAddr + uint64(pblsOff))
		if err != nil {
			return err
		}

		sr := io.NewSectionReader(f.r[uuid], int64(psetOffset), 1<<63-1)

		var pset PrebuiltLoaderSet
		if err := binary.Read(sr, binary.LittleEndian, &pset.prebuiltLoaderSetHeader); err != nil {
			return err
		}

		if pset.Magic != PrebuiltLoaderSetMagic {
			return fmt.Errorf("invalid magic for PrebuiltLoader at %#x: expected %x got %x", psetOffset, PrebuiltLoaderSetMagic, pset.Magic)
		}

		sr.Seek(int64(pset.LoadersArrayOffset), io.SeekStart)

		loaderOffsets := make([]uint32, pset.LoadersArrayCount)
		if err := binary.Read(sr, binary.LittleEndian, &loaderOffsets); err != nil {
			return err
		}

		for _, loaderOffset := range loaderOffsets {
			pbl, err := f.parsePrebuiltLoader(io.NewSectionReader(f.r[uuid], int64(psetOffset)+int64(loaderOffset), 1<<63-1))
			if err != nil {
				return err
			}
			pset.Loaders = append(pset.Loaders, *pbl)
		}

		if pset.CachePatchCount > 0 { // FIXME: this is in "/usr/bin/abmlite" but the values don't make sense (dyld_closure_util gets the same values)
			sr.Seek(int64(pset.CachePatchOffset), io.SeekStart)
			pset.Patches = make([]CachePatch, pset.CachePatchCount)
			if err := binary.Read(sr, binary.LittleEndian, &pset.Patches); err != nil {
				return err
			}
		}
		if pset.DyldCacheUuidOffset > 0 {
			sr.Seek(int64(pset.DyldCacheUuidOffset), io.SeekStart)
			var dcUUID types.UUID
			if err := binary.Read(sr, binary.LittleEndian, &dcUUID); err != nil {
				return err
			}
		}
		if pset.MustBeMissingPathsCount > 0 {
			sr.Seek(int64(pset.MustBeMissingPathsOffset), io.SeekStart)
			br := bufio.NewReader(sr)
			for i := 0; i < int(pset.MustBeMissingPathsCount); i++ {
				s, err := br.ReadString('\x00')
				if err != nil {
					return err
				}
				pset.MustBeMissingPaths = append(pset.MustBeMissingPaths, strings.TrimSuffix(s, "\x00"))
			}
		}
		if pset.ObjcSelectorHashTableOffset > 0 {

		}
		if pset.ObjcClassHashTableOffset > 0 {

		}
		if pset.ObjcProtocolHashTableOffset > 0 {

		}
		if pset.ObjcProtocolClassCacheOffset > 0 {

		}
		if pset.HasOptimizedSwift() {

		}

		handler(string(node.Data), &pset)
	}

	return nil
}

func (f *File) ForEachLaunchLoaderSetPath(handler func(execPath string)) error {
	if f.Headers[f.UUID].MappingOffset < uint32(unsafe.Offsetof(f.Headers[f.UUID].ProgramTrieSize)) {
		return ErrPrebuiltLoaderSetNotSupported
	}
	if f.Headers[f.UUID].ProgramTrieAddr == 0 {
		return ErrPrebuiltLoaderSetNotSupported
	}

	uuid, off, err := f.GetOffset(f.Headers[f.UUID].ProgramTrieAddr)
	if err != nil {
		return err
	}

	dat, err := f.ReadBytesForUUID(uuid, int64(off), uint64(f.Headers[f.UUID].ProgramTrieSize))
	if err != nil {
		return err
	}

	r := bytes.NewReader(dat)

	nodes, err := trie.ParseTrie(r)
	if err != nil {
		return err
	}

	for _, node := range nodes {
		handler(string(node.Data))
	}

	return nil
}

// GetLaunchLoaderSet returns the PrebuiltLoaderSet for the given executable app path.
func (f *File) GetLaunchLoaderSet(executablePath string) (*PrebuiltLoaderSet, error) {
	if f.Headers[f.UUID].MappingOffset < uint32(unsafe.Offsetof(f.Headers[f.UUID].ProgramTrieSize)) {
		return nil, ErrPrebuiltLoaderSetNotSupported
	}
	if f.Headers[f.UUID].ProgramTrieAddr == 0 {
		return nil, ErrPrebuiltLoaderSetNotSupported
	}

	var psetOffset uint64

	uuid, off, err := f.GetOffset(f.Headers[f.UUID].ProgramTrieAddr)
	if err != nil {
		return nil, err
	}

	dat, err := f.ReadBytesForUUID(uuid, int64(off), uint64(f.Headers[f.UUID].ProgramTrieSize))
	if err != nil {
		return nil, err
	}

	r := bytes.NewReader(dat)

	if _, err = trie.WalkTrie(r, executablePath); err != nil {
		return nil, fmt.Errorf("could not find executable %s in the ProgramTrie: %w", executablePath, err)
	}

	poolOffset, err := trie.ReadUleb128(r)
	if err != nil {
		return nil, err
	}

	uuid, psetOffset, err = f.GetOffset(f.Headers[f.UUID].ProgramsPblSetPoolAddr + uint64(poolOffset))
	if err != nil {
		return nil, err
	}

	sr := io.NewSectionReader(f.r[uuid], int64(psetOffset), 1<<63-1)

	var pset PrebuiltLoaderSet
	if err := binary.Read(sr, binary.LittleEndian, &pset.prebuiltLoaderSetHeader); err != nil {
		return nil, err
	}

	if pset.Magic != PrebuiltLoaderSetMagic {
		return nil, fmt.Errorf("invalid magic for PrebuiltLoader at %#x: expected %x got %x", psetOffset, PrebuiltLoaderSetMagic, pset.Magic)
	}

	sr.Seek(int64(pset.LoadersArrayOffset), io.SeekStart)

	loaderOffsets := make([]uint32, pset.LoadersArrayCount)
	if err := binary.Read(sr, binary.LittleEndian, &loaderOffsets); err != nil {
		return nil, err
	}

	for _, loaderOffset := range loaderOffsets {
		pbl, err := f.parsePrebuiltLoader(io.NewSectionReader(f.r[uuid], int64(psetOffset)+int64(loaderOffset), 1<<63-1))
		if err != nil {
			return nil, err
		}
		pset.Loaders = append(pset.Loaders, *pbl)
	}

	if pset.CachePatchCount > 0 {
		sr.Seek(int64(pset.CachePatchOffset), io.SeekStart)
		pset.Patches = make([]CachePatch, pset.CachePatchCount)
		if err := binary.Read(sr, binary.LittleEndian, &pset.Patches); err != nil {
			return nil, err
		}
	}
	if pset.DyldCacheUuidOffset > 0 {
		sr.Seek(int64(pset.DyldCacheUuidOffset), io.SeekStart)
		var dcUUID types.UUID
		if err := binary.Read(sr, binary.LittleEndian, &dcUUID); err != nil {
			return nil, err
		}
	}
	if pset.MustBeMissingPathsCount > 0 {
		sr.Seek(int64(pset.MustBeMissingPathsOffset), io.SeekStart)
		br := bufio.NewReader(sr)
		for i := 0; i < int(pset.MustBeMissingPathsCount); i++ {
			s, err := br.ReadString('\x00')
			if err != nil {
				return nil, err
			}
			pset.MustBeMissingPaths = append(pset.MustBeMissingPaths, strings.TrimSuffix(s, "\x00"))
		}
	}
	if pset.ObjcSelectorHashTableOffset > 0 {

	}
	if pset.ObjcClassHashTableOffset > 0 {

	}
	if pset.ObjcProtocolHashTableOffset > 0 {

	}
	if pset.ObjcProtocolClassCacheOffset > 0 {

	}
	if pset.HasOptimizedSwift() {

	}

	return &pset, nil
}

func (f *File) GetDylibPrebuiltLoader(executablePath string) (*PrebuiltLoader, error) {

	if f.Headers[f.UUID].MappingOffset < uint32(unsafe.Offsetof(f.Headers[f.UUID].ProgramTrieSize)) {
		return nil, ErrPrebuiltLoaderSetNotSupported
	}
	if f.Headers[f.UUID].MappingOffset < uint32(unsafe.Offsetof(f.Headers[f.UUID].DylibsPblSetAddr)) {
		return nil, ErrPrebuiltLoaderSetNotSupported
	}
	if f.Headers[f.UUID].DylibsPblSetAddr == 0 {
		return nil, ErrPrebuiltLoaderSetNotSupported
	}

	uuid, off, err := f.GetOffset(f.Headers[f.UUID].DylibsPblSetAddr)
	if err != nil {
		return nil, err
	}

	sr := io.NewSectionReader(f.r[uuid], int64(off), 1<<63-1)

	var pset PrebuiltLoaderSet
	if err := binary.Read(sr, binary.LittleEndian, &pset.prebuiltLoaderSetHeader); err != nil {
		return nil, err
	}

	sr.Seek(int64(pset.LoadersArrayOffset), io.SeekStart)

	loaderOffsets := make([]uint32, pset.LoadersArrayCount)
	if err := binary.Read(sr, binary.LittleEndian, &loaderOffsets); err != nil {
		return nil, err
	}

	imgIdx, err := f.HasImagePath(executablePath)
	if err != nil {
		return nil, err
	} else if imgIdx < 0 {
		return nil, fmt.Errorf("image not found")
	}

	sr.Seek(int64(loaderOffsets[imgIdx]), io.SeekStart)

	return f.parsePrebuiltLoader(io.NewSectionReader(f.r[uuid], int64(off)+int64(loaderOffsets[imgIdx]), 1<<63-1))
}

// parsePrebuiltLoader parses a prebuilt loader from a section reader.
func (f *File) parsePrebuiltLoader(sr *io.SectionReader) (*PrebuiltLoader, error) {
	var pbl PrebuiltLoader
	if err := binary.Read(sr, binary.LittleEndian, &pbl.prebuiltLoaderHeader); err != nil {
		return nil, err
	}

	if pbl.Magic != LoaderMagic {
		return nil, fmt.Errorf("invalid magic for prebuilt loader: expected %x got %x", LoaderMagic, pbl.Magic)
	}

	if pbl.PathOffset > 0 {
		sr.Seek(int64(pbl.PathOffset), io.SeekStart)
		br := bufio.NewReader(sr)
		path, err := br.ReadString('\x00')
		if err != nil {
			return nil, err
		}
		pbl.Path = strings.TrimSuffix(path, "\x00")
	}
	if pbl.AltPathOffset > 0 {
		sr.Seek(int64(pbl.AltPathOffset), io.SeekStart)
		br := bufio.NewReader(sr)
		path, err := br.ReadString('\x00')
		if err != nil {
			return nil, err
		}
		pbl.AltPath = strings.TrimSuffix(path, "\x00")
	}
	if pbl.FileValidationOffset > 0 {
		sr.Seek(int64(pbl.FileValidationOffset), io.SeekStart)
		var fv fileValidation
		if err := binary.Read(sr, binary.LittleEndian, &fv); err != nil {
			return nil, err
		}
		pbl.FileValidation = &fv
	}
	if pbl.RegionsCount() > 0 {
		sr.Seek(int64(pbl.RegionsOffset), io.SeekStart)
		pbl.Regions = make([]Region, pbl.RegionsCount())
		if err := binary.Read(sr, binary.LittleEndian, &pbl.Regions); err != nil {
			return nil, err
		}
	}
	if pbl.DependentLoaderRefsArrayOffset > 0 {
		sr.Seek(int64(pbl.DependentLoaderRefsArrayOffset), io.SeekStart)
		depsArray := make([]LoaderRef, pbl.DepCount)
		if err := binary.Read(sr, binary.LittleEndian, &depsArray); err != nil {
			return nil, err
		}
		kindsArray := make([]DependentKind, pbl.DepCount)
		if pbl.DependentKindArrayOffset > 0 {
			sr.Seek(int64(pbl.DependentKindArrayOffset), io.SeekStart)
			if err := binary.Read(sr, binary.LittleEndian, &kindsArray); err != nil {
				return nil, err
			}
		}
		for idx, dep := range depsArray {
			img := dep.String()
			if dep.Index() < uint16(len(f.Images)) {
				img = f.Images[dep.Index()].Name
			}
			pbl.Dependents = append(pbl.Dependents, dependent{
				Name: img,
				Kind: kindsArray[idx],
			})
		}
	}
	if pbl.BindTargetRefsCount > 0 {
		sr.Seek(int64(pbl.BindTargetRefsOffset), io.SeekStart)
		pbl.BindTargets = make([]BindTargetRef, pbl.BindTargetRefsCount)
		if err := binary.Read(sr, binary.LittleEndian, &pbl.BindTargets); err != nil {
			return nil, err
		}
	}
	if pbl.OverrideBindTargetRefsCount > 0 {
		sr.Seek(int64(pbl.OverrideBindTargetRefsOffset), io.SeekStart)
		pbl.OverrideBindTargets = make([]BindTargetRef, pbl.OverrideBindTargetRefsCount)
		if err := binary.Read(sr, binary.LittleEndian, &pbl.OverrideBindTargets); err != nil {
			return nil, err
		}
	}
	if pbl.ObjcBinaryInfoOffset > 0 {
		sr.Seek(int64(pbl.ObjcBinaryInfoOffset), io.SeekStart)
		var ofi ObjCBinaryInfo
		if err := binary.Read(sr, binary.LittleEndian, &ofi); err != nil {
			return nil, err
		}
		pbl.ObjcFixupInfo = &ofi
		sr.Seek(int64(pbl.ObjcBinaryInfoOffset)+int64(pbl.ObjcFixupInfo.ProtocolFixupsOffset), io.SeekStart)
		pbl.ObjcCanonicalProtocolFixups = make([]bool, pbl.ObjcFixupInfo.ProtocolListCount)
		if err := binary.Read(sr, binary.LittleEndian, &pbl.ObjcCanonicalProtocolFixups); err != nil {
			return nil, err
		}
		sr.Seek(int64(pbl.ObjcBinaryInfoOffset)+int64(pbl.ObjcFixupInfo.SelectorReferencesFixupsOffset), io.SeekStart)
		pbl.ObjcSelectorFixups = make([]BindTargetRef, pbl.ObjcFixupInfo.SelectorReferencesFixupsCount)
		if err := binary.Read(sr, binary.LittleEndian, &pbl.ObjcSelectorFixups); err != nil {
			return nil, err
		}
	}
	if pbl.IndexOfTwin != NoUnzipperedTwin {
		pbl.Twin = f.Images[pbl.IndexOfTwin].Name
	}
	if pbl.PatchTableOffset > 0 {
		sr.Seek(int64(pbl.PatchTableOffset), io.SeekStart)
		for {
			var patch DylibPatch
			if err := binary.Read(sr, binary.LittleEndian, &patch); err != nil {
				return nil, err
			}
			pbl.DylibPatches = append(pbl.DylibPatches, patch)
			if patch.Kind == endOfPatchTable {
				break
			}
		}
	}

	return &pbl, nil
}
