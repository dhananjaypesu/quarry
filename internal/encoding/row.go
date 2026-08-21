package encoding

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/dhananjaypesu/quarry/internal/types"
)

// Row encoding is deliberately different from key encoding. Keys must sort;
// values only have to round-trip, so they can use the cheaper length-prefixed
// form and skip the escaping that string keys need.
//
// Each stored column is (column id, tag, payload). Columns are addressed by id
// rather than by position, so adding a column to a table does not require
// rewriting rows that were written before it existed: the decoder simply finds
// no entry for that id and leaves the value NULL. Dropping a column is the
// same story in reverse — an id nothing asks about is skipped.

// EncodeRow serializes the values of a row. ids and vals must be parallel.
func EncodeRow(dst []byte, ids []uint32, vals []types.Value) []byte {
	for i, id := range ids {
		v := vals[i]
		if v.Null {
			// A NULL column is simply absent, which makes sparse rows cheap.
			continue
		}
		dst = binary.AppendUvarint(dst, uint64(id))
		switch v.Type {
		case types.Bool:
			if v.B {
				dst = append(dst, tagTrue)
			} else {
				dst = append(dst, tagFalse)
			}
		case types.Int:
			dst = append(dst, tagInt)
			dst = binary.AppendVarint(dst, v.I)
		case types.Float:
			dst = append(dst, tagFloat)
			dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(v.F))
		case types.Text:
			dst = append(dst, tagText)
			dst = binary.AppendUvarint(dst, uint64(len(v.S)))
			dst = append(dst, v.S...)
		}
	}
	return dst
}

// DecodeRow fills out with the values it finds, indexed by the position that
// idToPos gives each column id. Positions not present in the encoded row keep
// whatever the caller left there, which is why callers pre-fill out with typed
// NULLs before calling.
func DecodeRow(b []byte, idToPos map[uint32]int, out []types.Value) error {
	for len(b) > 0 {
		id, n := binary.Uvarint(b)
		if n <= 0 {
			return ErrCorruptKey
		}
		b = b[n:]
		if len(b) == 0 {
			return ErrCorruptKey
		}
		tag := b[0]
		b = b[1:]
		var v types.Value
		switch tag {
		case tagFalse:
			v = types.NewBool(false)
		case tagTrue:
			v = types.NewBool(true)
		case tagInt:
			i, n := binary.Varint(b)
			if n <= 0 {
				return ErrCorruptKey
			}
			b = b[n:]
			v = types.NewInt(i)
		case tagFloat:
			if len(b) < 8 {
				return ErrCorruptKey
			}
			v = types.NewFloat(math.Float64frombits(binary.LittleEndian.Uint64(b)))
			b = b[8:]
		case tagText:
			l, n := binary.Uvarint(b)
			if n <= 0 {
				return ErrCorruptKey
			}
			b = b[n:]
			if uint64(len(b)) < l {
				return ErrCorruptKey
			}
			v = types.NewText(string(b[:l]))
			b = b[l:]
		default:
			return fmt.Errorf("%w: row tag %#x", ErrCorruptKey, tag)
		}
		if pos, ok := idToPos[uint32(id)]; ok {
			out[pos] = v
		}
	}
	return nil
}
