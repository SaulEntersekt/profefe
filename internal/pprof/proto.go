package pprof

import (
	"compress/gzip"
	"io"
	"time"
)

// A ProfileBuilder writes a profile incrementally from a
// stream of profile samples delivered by the runtime.
type ProfileBuilder struct {
	start      time.Time
	end        time.Time
	havePeriod bool
	period     int64
	m          profMap
	locMap     LocMap

	// encoding state
	w         io.Writer
	zw        *gzip.Writer
	pb        protobuf
	strings   []string
	stringMap map[string]int
	locs      map[uint64]int
	funcs     map[string]int // Package path-qualified function name to Function.ID
	mem       []memMap
}

type memMap struct {
	// initialized as reading mapping
	start         uint64
	end           uint64
	offset        uint64
	file, buildID string

	funcs symbolizeFlag
	fake  bool // map entry was faked; /proc/self/maps wasn't available
}

// symbolizeFlag keeps track of symbolization result.
//   0                  : no symbol lookup was performed
//   1<<0 (lookupTried) : symbol lookup was performed
//   1<<1 (lookupFailed): symbol lookup was performed but failed
type symbolizeFlag uint8

const (
	lookupTried  symbolizeFlag = 1 << iota
	lookupFailed symbolizeFlag = 1 << iota
)

const (
	// message Profile
	tagProfile_SampleType        = 1  // repeated ValueType
	tagProfile_Sample            = 2  // repeated Sample
	tagProfile_Mapping           = 3  // repeated Mapping
	tagProfile_Location          = 4  // repeated Location
	tagProfile_Function          = 5  // repeated Function
	tagProfile_StringTable       = 6  // repeated string
	tagProfile_DropFrames        = 7  // int64 (string table index)
	tagProfile_KeepFrames        = 8  // int64 (string table index)
	tagProfile_TimeNanos         = 9  // int64
	tagProfile_DurationNanos     = 10 // int64
	tagProfile_PeriodType        = 11 // ValueType (really optional string???)
	tagProfile_Period            = 12 // int64
	tagProfile_Comment           = 13 // repeated int64
	tagProfile_DefaultSampleType = 14 // int64

	// message ValueType
	tagValueType_Type = 1 // int64 (string table index)
	tagValueType_Unit = 2 // int64 (string table index)

	// message Sample
	tagSample_Location = 1 // repeated uint64
	tagSample_Value    = 2 // repeated int64
	tagSample_Label    = 3 // repeated Label

	// message Label
	tagLabel_Key = 1 // int64 (string table index)
	tagLabel_Str = 2 // int64 (string table index)
	tagLabel_Num = 3 // int64

	// message Mapping
	tagMapping_ID              = 1  // uint64
	tagMapping_Start           = 2  // uint64
	tagMapping_Limit           = 3  // uint64
	tagMapping_Offset          = 4  // uint64
	tagMapping_Filename        = 5  // int64 (string table index)
	tagMapping_BuildID         = 6  // int64 (string table index)
	tagMapping_HasFunctions    = 7  // bool
	tagMapping_HasFilenames    = 8  // bool
	tagMapping_HasLineNumbers  = 9  // bool
	tagMapping_HasInlineFrames = 10 // bool

	// message Location
	tagLocation_ID        = 1 // uint64
	tagLocation_MappingID = 2 // uint64
	tagLocation_Address   = 3 // uint64
	tagLocation_Line      = 4 // repeated Line

	// message Line
	tagLine_FunctionID = 1 // uint64
	tagLine_Line       = 2 // int64

	// message Function
	tagFunction_ID         = 1 // uint64
	tagFunction_Name       = 2 // int64 (string table index)
	tagFunction_SystemName = 3 // int64 (string table index)
	tagFunction_Filename   = 4 // int64 (string table index)
	tagFunction_StartLine  = 5 // int64
)

// stringIndex adds s to the string table if not already present
// and returns the index of s in the string table.
func (b *ProfileBuilder) stringIndex(s string) int64 {
	id, ok := b.stringMap[s]
	if !ok {
		id = len(b.strings)
		b.strings = append(b.strings, s)
		b.stringMap[s] = id
	}
	return int64(id)
}

func (b *ProfileBuilder) flush() {
	const dataFlush = 4096
	if b.pb.nest == 0 && len(b.pb.data) > dataFlush {
		b.zw.Write(b.pb.data)
		b.pb.data = b.pb.data[:0]
	}
}

// pbValueType encodes a ValueType message to b.pb.
func (b *ProfileBuilder) pbValueType(tag int, typ, unit string) {
	start := b.pb.startMessage()
	b.pb.int64(tagValueType_Type, b.stringIndex(typ))
	b.pb.int64(tagValueType_Unit, b.stringIndex(unit))
	b.pb.endMessage(tag, start)
}

// pbSample encodes a Sample message to b.pb.
func (b *ProfileBuilder) pbSample(values []int64, locs []uint64, labels func()) {
	start := b.pb.startMessage()
	b.pb.int64s(tagSample_Value, values)
	b.pb.uint64s(tagSample_Location, locs)
	if labels != nil {
		labels()
	}
	b.pb.endMessage(tagProfile_Sample, start)
	b.flush()
}

// pbLabel encodes a Label message to b.pb.
func (b *ProfileBuilder) pbLabel(tag int, key, str string, num int64) {
	start := b.pb.startMessage()
	b.pb.int64Opt(tagLabel_Key, b.stringIndex(key))
	b.pb.int64Opt(tagLabel_Str, b.stringIndex(str))
	b.pb.int64Opt(tagLabel_Num, num)
	b.pb.endMessage(tag, start)
}

// pbLine encodes a Line message to b.pb.
func (b *ProfileBuilder) pbLine(tag int, funcID uint64, line int64) {
	start := b.pb.startMessage()
	b.pb.uint64Opt(tagLine_FunctionID, funcID)
	b.pb.int64Opt(tagLine_Line, line)
	b.pb.endMessage(tag, start)
}

// pbMapping encodes a Mapping message to b.pb.
func (b *ProfileBuilder) pbMapping(tag int, id, base, limit, offset uint64, file, buildID string, hasFuncs bool) {
	start := b.pb.startMessage()
	b.pb.uint64Opt(tagMapping_ID, id)
	b.pb.uint64Opt(tagMapping_Start, base)
	b.pb.uint64Opt(tagMapping_Limit, limit)
	b.pb.uint64Opt(tagMapping_Offset, offset)
	b.pb.int64Opt(tagMapping_Filename, b.stringIndex(file))
	b.pb.int64Opt(tagMapping_BuildID, b.stringIndex(buildID))
	// TODO: we set HasFunctions if all symbols from samples were symbolized (hasFuncs).
	// Decide what to do about HasInlineFrames and HasLineNumbers.
	// Also, another approach to handle the mapping entry with
	// incomplete symbolization results is to dupliace the mapping
	// entry (but with different Has* fields values) and use
	// different entries for symbolized locations and unsymbolized locations.
	if hasFuncs {
		b.pb.bool(tagMapping_HasFunctions, true)
	}
	b.pb.endMessage(tag, start)
}

// locForPC returns the location ID for addr.
// addr must a return PC or 1 + the PC of an inline marker. This returns the location of the corresponding call.
// It may emit to b.pb, so there must be no message encoding in progress.
func (b *ProfileBuilder) locForPC(addr uint64) uint64 {
	id := uint64(b.locs[addr])
	if id != 0 {
		return id
	}

	symbolizeResult := lookupTried

	// We can't write out functions while in the middle of the
	// Location message, so record new functions we encounter and
	// write them out after the Location.
	type newFunc struct {
		id         uint64
		name, file string
	}
	newFuncs := make([]newFunc, 0, 8)

	id = uint64(len(b.locs)) + 1
	b.locs[addr] = int(id)
	start := b.pb.startMessage()
	b.pb.uint64Opt(tagLocation_ID, id)
	b.pb.uint64Opt(tagLocation_Address, uint64(0))

	if frame, ok := b.locMap[addr]; ok {
		funcID := uint64(b.funcs[frame.Function])
		if funcID == 0 {
			funcID = uint64(len(b.funcs)) + 1
			b.funcs[frame.Function] = int(funcID)
			newFuncs = append(newFuncs, newFunc{funcID, frame.Function, frame.File})
		}
		b.pbLine(tagLocation_Line, funcID, int64(frame.Line))
	}
	for i := range b.mem {
		if b.mem[i].start <= addr && addr < b.mem[i].end || b.mem[i].fake {
			b.pb.uint64Opt(tagLocation_MappingID, uint64(i+1))

			m := b.mem[i]
			m.funcs |= symbolizeResult
			b.mem[i] = m
			break
		}
	}
	b.pb.endMessage(tagProfile_Location, start)

	// Write out functions we found during frame expansion.
	for _, fn := range newFuncs {
		start := b.pb.startMessage()
		b.pb.uint64Opt(tagFunction_ID, fn.id)
		b.pb.int64Opt(tagFunction_Name, b.stringIndex(fn.name))
		b.pb.int64Opt(tagFunction_SystemName, b.stringIndex(fn.name))
		b.pb.int64Opt(tagFunction_Filename, b.stringIndex(fn.file))
		b.pb.endMessage(tagProfile_Function, start)
	}

	b.flush()
	return id
}

// NewProfileBuilder returns a new ProfileBuilder.
// CPU profiling data obtained from the runtime can be added
// by calling b.addCPUData, and then the eventual profile
// can be obtained by calling b.finish.
func NewProfileBuilder(w io.Writer, locMap LocMap) *ProfileBuilder {
	zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	b := &ProfileBuilder{
		w:         w,
		zw:        zw,
		start:     time.Now(),
		locMap:    locMap,
		strings:   []string{""},
		stringMap: map[string]int{"": 0},
		locs:      map[uint64]int{},
		funcs:     map[string]int{},
	}
	b.readMapping()
	return b
}

// build completes and returns the constructed profile.
func (b *ProfileBuilder) build() {
	b.end = time.Now()

	b.pb.int64Opt(tagProfile_TimeNanos, b.start.UnixNano())
	if b.havePeriod { // must be CPU profile
		b.pbValueType(tagProfile_SampleType, "samples", "count")
		b.pbValueType(tagProfile_SampleType, "cpu", "nanoseconds")
		b.pb.int64Opt(tagProfile_DurationNanos, b.end.Sub(b.start).Nanoseconds())
		b.pbValueType(tagProfile_PeriodType, "cpu", "nanoseconds")
		b.pb.int64Opt(tagProfile_Period, b.period)
	}

	//values := []int64{0, 0}
	//var locs []uint64
	//for e := b.m.all; e != nil; e = e.nextAll {
	//	values[0] = e.count
	//	values[1] = e.count * b.period
	//
	//	var labels func()
	//	if e.tag != nil {
	//		labels = func() {
	//			for k, v := range *(*labelMap)(e.tag) {
	//				b.pbLabel(tagSample_Label, k, v, 0)
	//			}
	//		}
	//	}
	//
	//	locs = locs[:0]
	//	for i, addr := range e.stk {
	//		// Addresses from stack traces point to the
	//		// next instruction after each call, except
	//		// for the leaf, which points to where the
	//		// signal occurred. locForPC expects return
	//		// PCs, so increment the leaf address to look
	//		// like a return PC.
	//		if i == 0 {
	//			addr++
	//		}
	//		l := b.locForPC(addr)
	//		if l == 0 { // runtime.goexit
	//			continue
	//		}
	//		locs = append(locs, l)
	//	}
	//	b.pbSample(values, locs, labels)
	//}

	for i, m := range b.mem {
		hasFunctions := m.funcs == lookupTried // lookupTried but not lookupFailed
		b.pbMapping(tagProfile_Mapping, uint64(i+1), uint64(m.start), uint64(m.end), m.offset, m.file, m.buildID, hasFunctions)
	}

	// TODO: Anything for tagProfile_DropFrames?
	// TODO: Anything for tagProfile_KeepFrames?

	b.pb.strings(tagProfile_StringTable, b.strings)
	b.zw.Write(b.pb.data)
	b.zw.Close()
}

// readMapping reads /proc/self/maps and writes mappings to b.pb.
// It saves the address ranges of the mappings in b.mem for use
// when emitting locations.
func (b *ProfileBuilder) readMapping() {
	b.addMappingEntry(0, 0, 0, "", "", true)
	// TODO(hyangah): make addMapping return *memMap or
	// take a memMap struct, and get rid of addMappingEntry
	// that takes a bunch of positional arguments.
}

func (b *ProfileBuilder) addMapping(lo, hi, offset uint64, file, buildID string) {
	b.addMappingEntry(lo, hi, offset, file, buildID, false)
}

func (b *ProfileBuilder) addMappingEntry(lo, hi, offset uint64, file, buildID string, fake bool) {
	b.mem = append(b.mem, memMap{
		start:   lo,
		end:     hi,
		offset:  offset,
		file:    file,
		buildID: buildID,
		fake:    fake,
	})
}
