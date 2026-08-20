package pack

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// Byte layout of a `.sptbl` table, specified by ADR 0029.
const (
	tableMagic        = "SPK1"
	tableFormatV1     = 1
	tableHeaderBytes  = 40
	tableKeyEntrySize = 12
	// Bit 0 of the header's flags field: the key index is present.
	tableFlagKeyIndex = 1
)

// table is one validated `.sptbl` file, still in its on-disk form.
type table struct {
	name      string
	bytes     []byte
	rowTypeID int32
	rowCount  uint32

	keyIndexOffset uint32
	rowIndexOffset uint32
	rowDataOffset  uint32
}

// openTable applies every structural rule ADR 0029 requires a reader to check
// before the shard accepts a pack. There is no partial load and no repair path,
// so every failure here is fatal to startup.
func openTable(name string, payload []byte) (*table, error) {
	fail := func(format string, arguments ...any) (*table, error) {
		return nil, fmt.Errorf("%w: table %q %s", ErrMalformedTable, name, fmt.Sprintf(format, arguments...))
	}

	if len(payload) < tableHeaderBytes {
		return fail("is %d bytes, shorter than the %d-byte header", len(payload), tableHeaderBytes)
	}
	if string(payload[:4]) != tableMagic {
		return fail("does not start with the %s magic", tableMagic)
	}
	if version := binary.LittleEndian.Uint16(payload[4:]); version != tableFormatV1 {
		return fail("declares format_version %d, want %d", version, tableFormatV1)
	}
	flags := binary.LittleEndian.Uint16(payload[6:])
	if flags&^uint16(tableFlagKeyIndex) != 0 {
		return fail("sets reserved flag bits %#04x", flags)
	}
	if flags&tableFlagKeyIndex == 0 {
		return fail("has no key index")
	}
	for _, reserved := range payload[32:40] {
		if reserved != 0 {
			return fail("has non-zero reserved header bytes")
		}
	}

	loaded := &table{
		name:           name,
		bytes:          payload,
		rowTypeID:      int32(binary.LittleEndian.Uint32(payload[8:])),
		rowCount:       binary.LittleEndian.Uint32(payload[12:]),
		keyIndexOffset: binary.LittleEndian.Uint32(payload[16:]),
		rowIndexOffset: binary.LittleEndian.Uint32(payload[20:]),
		rowDataOffset:  binary.LittleEndian.Uint32(payload[24:]),
	}
	rowDataBytes := binary.LittleEndian.Uint32(payload[28:])

	// Every region length is computed in uint64. In uint32 the products wrap:
	// row_count 0x40000000 makes the key index 12*row_count = 0 bytes long and
	// the row index 4*(row_count+1) = 4, both of which fit a 48-byte file, and
	// the row-index walk below then runs off the end of the payload. The Rust
	// verifier uses checked arithmetic for the same reason, and the two readers
	// must accept exactly the same set of files.
	regions := []struct {
		name   string
		offset uint32
		length uint64
	}{
		{"key index", loaded.keyIndexOffset, uint64(tableKeyEntrySize) * uint64(loaded.rowCount)},
		{"row index", loaded.rowIndexOffset, 4 * (uint64(loaded.rowCount) + 1)},
		{"row data", loaded.rowDataOffset, uint64(rowDataBytes)},
	}
	for _, region := range regions {
		if region.offset < tableHeaderBytes {
			return fail("%s at offset %d overlaps the header", region.name, region.offset)
		}
		end := uint64(region.offset) + region.length
		if end > uint64(len(payload)) {
			return fail("%s ends at %d, past the %d byte file", region.name, end, len(payload))
		}
	}
	for _, pair := range [][2]int{{0, 1}, {0, 2}, {1, 2}} {
		first, second := regions[pair[0]], regions[pair[1]]
		if first.length == 0 || second.length == 0 {
			continue
		}
		if uint64(first.offset) < uint64(second.offset)+second.length &&
			uint64(second.offset) < uint64(first.offset)+first.length {
			return fail("%s and %s regions overlap", first.name, second.name)
		}
	}
	if uint64(loaded.rowDataOffset)+uint64(rowDataBytes) != uint64(len(payload)) {
		return fail("is %d bytes, want exactly %d", len(payload), uint64(loaded.rowDataOffset)+uint64(rowDataBytes))
	}

	var previous uint32
	for index := uint32(0); index <= loaded.rowCount; index++ {
		value := binary.LittleEndian.Uint32(payload[loaded.rowIndexOffset+index*4:])
		switch {
		case index == 0 && value != 0:
			return fail("row index starts at %d, want 0", value)
		case index > 0 && value <= previous:
			return fail("row index is not strictly increasing at entry %d", index)
		}
		previous = value
	}
	if previous != rowDataBytes {
		return fail("row index ends at %d, want row_data_bytes %d", previous, rowDataBytes)
	}

	var previousHash uint64
	var previousOrdinal uint32
	for index := uint32(0); index < loaded.rowCount; index++ {
		hash, ordinal := loaded.keyEntry(index)
		if ordinal >= loaded.rowCount {
			return fail("key index entry %d points at row %d, past row_count %d", index, ordinal, loaded.rowCount)
		}
		if index > 0 && (hash < previousHash || (hash == previousHash && ordinal < previousOrdinal)) {
			return fail("key index is not sorted at entry %d", index)
		}
		previousHash, previousOrdinal = hash, ordinal
	}
	if loaded.rowCount == 0 && rowDataBytes != 0 {
		return fail("has no rows but %d bytes of row data", rowDataBytes)
	}
	return loaded, nil
}

func (t *table) keyEntry(index uint32) (uint64, uint32) {
	base := t.keyIndexOffset + index*tableKeyEntrySize
	return binary.LittleEndian.Uint64(t.bytes[base:]), binary.LittleEndian.Uint32(t.bytes[base+8:])
}

// row borrows the encoded bytes of one row by ordinal.
func (t *table) row(ordinal uint32) []byte {
	start := binary.LittleEndian.Uint32(t.bytes[t.rowIndexOffset+ordinal*4:])
	end := binary.LittleEndian.Uint32(t.bytes[t.rowIndexOffset+(ordinal+1)*4:])
	return t.bytes[t.rowDataOffset+start : t.rowDataOffset+end]
}

// rows borrows every encoded row in ordinal order.
func (t *table) rows() [][]byte {
	all := make([][]byte, 0, t.rowCount)
	for ordinal := uint32(0); ordinal < t.rowCount; ordinal++ {
		all = append(all, t.row(ordinal))
	}
	return all
}

// candidates returns the rows whose key hash matches `canonicalID`, without
// decoding the rest of the table. Hash collisions are legal, so a caller
// compares the decoded id of each candidate.
func (t *table) candidates(canonicalID string) [][]byte {
	wanted := keyHash(canonicalID)
	first := sort.Search(int(t.rowCount), func(index int) bool {
		hash, _ := t.keyEntry(uint32(index))
		return hash >= wanted
	})
	var matches [][]byte
	for index := uint32(first); index < t.rowCount; index++ {
		hash, ordinal := t.keyEntry(index)
		if hash != wanted {
			break
		}
		matches = append(matches, t.row(ordinal))
	}
	return matches
}
