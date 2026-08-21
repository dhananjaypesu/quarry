// Package encoding turns SQL values into the byte strings the storage engine
// sorts by, and back again.
//
// This is the layer where a relational engine earns its range scans. lsmdb
// exposes exactly one ordering — bytewise over keys — so every access path the
// planner can choose has to be expressible as a byte range. That forces the
// encoding to be *order preserving*: for two values of the same type,
// bytes.Compare of their encodings must agree with SQL's comparison. Get that
// wrong and an index scan silently skips rows, which is the kind of bug that
// only shows up on the rows you did not test.
package encoding

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/dhananjaypesu/quarry/internal/types"
)

// Keyspace prefixes. Every key the engine writes begins with one of these, so
// the three namespaces occupy disjoint, contiguous stretches of the store and
// a scan of one can never wander into another.
const (
	PrefixMeta  byte = 0x01 // catalog: schemas and the id counter
	PrefixRow   byte = 0x02 // table data, keyed by primary key
	PrefixIndex byte = 0x03 // secondary index entries
)

// Order-preserving type tags. NULL sorts before every real value because its
// tag is the smallest, which is what makes NULLS FIRST fall out of the
// encoding rather than needing a special case in the scan.
const (
	tagNull  byte = 0x00
	tagFalse byte = 0x01
	tagTrue  byte = 0x02
	tagInt   byte = 0x03
	tagFloat byte = 0x04
	tagText  byte = 0x05
)

// TableRowPrefix is the prefix shared by every row of a table: a full table
// scan is the range [prefix, prefixEnd).
func TableRowPrefix(tableID uint32) []byte {
	b := make([]byte, 0, 5)
	b = append(b, PrefixRow)
	return binary.BigEndian.AppendUint32(b, tableID)
}

// IndexPrefix is the prefix shared by every entry of one index.
func IndexPrefix(indexID uint32) []byte {
	b := make([]byte, 0, 5)
	b = append(b, PrefixIndex)
	return binary.BigEndian.AppendUint32(b, indexID)
}

// MetaKey builds a catalog key. Catalog entries are ordinary rows in the same
// store as the data, so a schema change and the writes that depend on it can
// be made atomic by the same batch.
func MetaKey(parts ...string) []byte {
	b := []byte{PrefixMeta}
	for _, p := range parts {
		b = append(b, p...)
		b = append(b, 0x00)
	}
	return b
}

// PrefixEnd returns the exclusive upper bound of the range covering every key
// that starts with prefix — the prefix with its last non-0xff byte
// incremented. A nil result means the prefix is all 0xff and the range runs to
// the end of the keyspace.
func PrefixEnd(prefix []byte) []byte {
	end := make([]byte, len(prefix))
	copy(end, prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// EncodeKeyValue appends the order-preserving encoding of v to dst.
//
// Integers are stored big-endian with the sign bit flipped, which maps the
// signed range onto the unsigned range monotonically so that -1 sorts before
// 0. Floats get the same treatment plus the standard IEEE-754 trick: negative
// values have every bit inverted, positive values only the sign bit set, after
// which their bit patterns are ordered like the numbers they represent.
// Strings are escaped so that a value containing 0x00 cannot forge the
// terminator and make a shorter string look longer.
func EncodeKeyValue(dst []byte, v types.Value) []byte {
	if v.Null {
		return append(dst, tagNull)
	}
	switch v.Type {
	case types.Bool:
		if v.B {
			return append(dst, tagTrue)
		}
		return append(dst, tagFalse)
	case types.Int:
		dst = append(dst, tagInt)
		return binary.BigEndian.AppendUint64(dst, uint64(v.I)^(1<<63))
	case types.Float:
		dst = append(dst, tagFloat)
		bits := math.Float64bits(v.F)
		if bits&(1<<63) != 0 {
			bits = ^bits
		} else {
			bits |= 1 << 63
		}
		return binary.BigEndian.AppendUint64(dst, bits)
	case types.Text:
		dst = append(dst, tagText)
		return appendEscaped(dst, v.S)
	}
	return append(dst, tagNull)
}

// EncodeKeyValues encodes a tuple of values, which is what a composite index
// key or a multi-column primary key is.
func EncodeKeyValues(dst []byte, vals ...types.Value) []byte {
	for _, v := range vals {
		dst = EncodeKeyValue(dst, v)
	}
	return dst
}

// appendEscaped writes a string as escaped bytes followed by a terminator.
// 0x00 becomes 0x00 0xff; the terminator is 0x00 0x01. Because the escape byte
// (0xff) sorts above the terminator byte (0x01), a string that continues past
// where another ends still sorts after it.
func appendEscaped(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x00 {
			dst = append(dst, 0x00, 0xff)
			continue
		}
		dst = append(dst, s[i])
	}
	return append(dst, 0x00, 0x01)
}

// ErrCorruptKey is returned when a key cannot be decoded, which means the
// store holds bytes this build did not write.
var ErrCorruptKey = errors.New("quarry: corrupt key encoding")

// DecodeKeyValue reads one value from the front of b, returning it and the
// remainder. Index scans need this to recover the primary key appended to each
// index entry without a second lookup.
func DecodeKeyValue(b []byte) (types.Value, []byte, error) {
	if len(b) == 0 {
		return types.Value{}, nil, ErrCorruptKey
	}
	tag, rest := b[0], b[1:]
	switch tag {
	case tagNull:
		return types.NullValue, rest, nil
	case tagFalse:
		return types.NewBool(false), rest, nil
	case tagTrue:
		return types.NewBool(true), rest, nil
	case tagInt:
		if len(rest) < 8 {
			return types.Value{}, nil, ErrCorruptKey
		}
		u := binary.BigEndian.Uint64(rest) ^ (1 << 63)
		return types.NewInt(int64(u)), rest[8:], nil
	case tagFloat:
		if len(rest) < 8 {
			return types.Value{}, nil, ErrCorruptKey
		}
		bits := binary.BigEndian.Uint64(rest)
		if bits&(1<<63) != 0 {
			bits &^= 1 << 63
		} else {
			bits = ^bits
		}
		return types.NewFloat(math.Float64frombits(bits)), rest[8:], nil
	case tagText:
		var out []byte
		for i := 0; i < len(rest); i++ {
			if rest[i] != 0x00 {
				out = append(out, rest[i])
				continue
			}
			if i+1 >= len(rest) {
				return types.Value{}, nil, ErrCorruptKey
			}
			switch rest[i+1] {
			case 0x01:
				return types.NewText(string(out)), rest[i+2:], nil
			case 0xff:
				out = append(out, 0x00)
				i++
			default:
				return types.Value{}, nil, ErrCorruptKey
			}
		}
		return types.Value{}, nil, ErrCorruptKey
	}
	return types.Value{}, nil, fmt.Errorf("%w: tag %#x", ErrCorruptKey, tag)
}
