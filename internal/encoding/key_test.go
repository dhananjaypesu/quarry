package encoding

import (
	"bytes"
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/dhananjaypesu/quarry/internal/types"
)

// The whole access-path story rests on one property: for values of the same
// type, the byte order of their encodings is their SQL order. Every range scan
// the planner emits is wrong if this does not hold, so it is tested by
// exhaustive comparison over a randomized sample rather than by a few
// hand-picked pairs.
func TestKeyEncodingIsOrderPreserving(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	sets := map[string][]types.Value{}

	ints := []types.Value{types.NewInt(math.MinInt64), types.NewInt(-1), types.NewInt(0), types.NewInt(1), types.NewInt(math.MaxInt64)}
	floats := []types.Value{types.NewFloat(math.Inf(-1)), types.NewFloat(-1e300), types.NewFloat(-0.5), types.NewFloat(0), types.NewFloat(0.5), types.NewFloat(1e300), types.NewFloat(math.Inf(1))}
	texts := []types.Value{types.NewText(""), types.NewText("a"), types.NewText("aa"), types.NewText("ab"), types.NewText("b"), types.NewText("b\x00c"), types.NewText("\xff")}
	for i := 0; i < 200; i++ {
		ints = append(ints, types.NewInt(rng.Int63()-(1<<62)))
		floats = append(floats, types.NewFloat((rng.Float64()-0.5)*math.Pow(10, float64(rng.Intn(40)-20))))
		b := make([]byte, rng.Intn(12))
		for j := range b {
			b[j] = byte(rng.Intn(256))
		}
		texts = append(texts, types.NewText(string(b)))
	}
	sets["int"], sets["float"], sets["text"] = ints, floats, texts

	for name, vals := range sets {
		for i := range vals {
			for j := range vals {
				want, ok := types.Compare(vals[i], vals[j])
				if !ok {
					t.Fatalf("%s: unexpected NULL comparison", name)
				}
				got := bytes.Compare(EncodeKeyValue(nil, vals[i]), EncodeKeyValue(nil, vals[j]))
				if sign(got) != sign(want) {
					t.Fatalf("%s: %v vs %v: value order %d, byte order %d",
						name, vals[i], vals[j], want, got)
				}
			}
		}
	}
}

func sign(i int) int {
	switch {
	case i < 0:
		return -1
	case i > 0:
		return 1
	}
	return 0
}

func TestNullSortsFirst(t *testing.T) {
	null := EncodeKeyValue(nil, types.NullValue)
	for _, v := range []types.Value{types.NewInt(math.MinInt64), types.NewText(""), types.NewBool(false), types.NewFloat(math.Inf(-1))} {
		if bytes.Compare(null, EncodeKeyValue(nil, v)) >= 0 {
			t.Fatalf("NULL does not sort before %v", v)
		}
	}
}

func TestKeyValueRoundTrip(t *testing.T) {
	vals := []types.Value{
		types.NullValue, types.NewBool(true), types.NewBool(false),
		types.NewInt(-99), types.NewInt(0), types.NewInt(1 << 40),
		types.NewFloat(-2.5), types.NewFloat(3.25),
		types.NewText(""), types.NewText("hello"), types.NewText("has\x00nul"), types.NewText("\x00\xff\x01"),
	}
	// Encoding the whole tuple at once also checks that each decoder stops
	// exactly where the next value begins.
	buf := EncodeKeyValues(nil, vals...)
	rest := buf
	for i, want := range vals {
		got, tail, err := DecodeKeyValue(rest)
		if err != nil {
			t.Fatalf("value %d: %v", i, err)
		}
		rest = tail
		if got.Null != want.Null || got.String() != want.String() {
			t.Fatalf("value %d: got %v, want %v", i, got, want)
		}
	}
	if len(rest) != 0 {
		t.Fatalf("%d bytes left over", len(rest))
	}
}

func TestRowRoundTripAndSparseColumns(t *testing.T) {
	ids := []uint32{1, 2, 3, 4}
	vals := []types.Value{types.NewInt(7), types.NullValue, types.NewText("x"), types.NewFloat(1.5)}
	enc := EncodeRow(nil, ids, vals)

	posMap := map[uint32]int{1: 0, 2: 1, 3: 2, 4: 3}
	out := []types.Value{types.NullOf(types.Int), types.NullOf(types.Text), types.NullOf(types.Text), types.NullOf(types.Float)}
	if err := DecodeRow(enc, posMap, out); err != nil {
		t.Fatal(err)
	}
	for i := range vals {
		if out[i].String() != vals[i].String() {
			t.Fatalf("column %d: got %v, want %v", i, out[i], vals[i])
		}
	}
	// A column the reader does not know about is skipped rather than
	// corrupting the columns after it, which is what makes a dropped column
	// safe.
	partial := []types.Value{types.NullOf(types.Int), types.NullOf(types.Float)}
	if err := DecodeRow(enc, map[uint32]int{1: 0, 4: 1}, partial); err != nil {
		t.Fatal(err)
	}
	if partial[0].I != 7 || partial[1].F != 1.5 {
		t.Fatalf("sparse decode got %v, %v", partial[0], partial[1])
	}
}

func TestPrefixEnd(t *testing.T) {
	cases := []struct{ in, want []byte }{
		{[]byte{1, 2, 3}, []byte{1, 2, 4}},
		{[]byte{1, 2, 0xff}, []byte{1, 3}},
		{[]byte{0xff, 0xff}, nil},
	}
	for _, c := range cases {
		if got := PrefixEnd(c.in); !bytes.Equal(got, c.want) {
			t.Fatalf("PrefixEnd(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// A prefix range must cover every key that starts with it and nothing else,
// which is the contract a full table scan depends on.
func TestPrefixRangeCoversExactly(t *testing.T) {
	prefix := TableRowPrefix(42)
	end := PrefixEnd(prefix)
	var inside, outside [][]byte
	for i := 0; i < 50; i++ {
		inside = append(inside, EncodeKeyValue(append([]byte(nil), prefix...), types.NewInt(int64(i-25))))
	}
	outside = append(outside, TableRowPrefix(41), TableRowPrefix(43), IndexPrefix(42))
	all := append(append([][]byte{}, inside...), outside...)
	sort.Slice(all, func(i, j int) bool { return bytes.Compare(all[i], all[j]) < 0 })
	for _, k := range inside {
		if bytes.Compare(k, prefix) < 0 || bytes.Compare(k, end) >= 0 {
			t.Fatalf("key %x is outside its own table range", k)
		}
	}
	for _, k := range outside {
		if bytes.Compare(k, prefix) >= 0 && bytes.Compare(k, end) < 0 {
			t.Fatalf("key %x from another namespace falls inside the range", k)
		}
	}
}
