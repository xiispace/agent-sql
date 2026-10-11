package copier

import (
	"bytes"
	"math"
	"math/big"
	"strconv"

	"github.com/shhac/agent-sql/internal/schemadiff"
)

// keyCmp compares two rows by their key columns, in the order drivers sort
// them for that key kind (see driver.ReadRequest.OrderBy).
type keyCmp struct {
	idx   []int
	kinds []schemadiff.KeyKind
}

func (k keyCmp) compare(a, b [][]byte) int {
	for i, col := range k.idx {
		if c := compareValue(k.kinds[i], a[col], b[col]); c != 0 {
			return c
		}
	}
	return 0
}

func compareValue(kind schemadiff.KeyKind, a, b []byte) int {
	switch kind {
	case schemadiff.KeyInteger:
		return compareIntegers(a, b)
	case schemadiff.KeyDecimal:
		ra, okA := new(big.Rat).SetString(string(a))
		rb, okB := new(big.Rat).SetString(string(b))
		if okA && okB {
			return ra.Cmp(rb)
		}
	case schemadiff.KeyFloat:
		fa, errA := strconv.ParseFloat(string(a), 64)
		fb, errB := strconv.ParseFloat(string(b), 64)
		if errA == nil && errB == nil {
			// ORDER BY sorts NaN after every number (PostgreSQL; MySQL
			// stores none), and equal only to itself.
			switch na, nb := math.IsNaN(fa), math.IsNaN(fb); {
			case na && nb:
				return 0
			case na:
				return 1
			case nb:
				return -1
			}
			switch {
			case fa < fb:
				return -1
			case fa > fb:
				return 1
			}
			return 0
		}
	}
	return bytes.Compare(a, b)
}

// compareIntegers compares canonical decimal integer text (no leading zeros,
// optional minus sign) without parsing, so unsigned 64-bit values work too.
func compareIntegers(a, b []byte) int {
	negA := len(a) > 0 && a[0] == '-'
	negB := len(b) > 0 && b[0] == '-'
	switch {
	case negA && !negB:
		return -1
	case !negA && negB:
		return 1
	}
	c := 0
	switch {
	case len(a) < len(b):
		c = -1
	case len(a) > len(b):
		c = 1
	default:
		c = bytes.Compare(a, b)
	}
	if negA {
		return -c
	}
	return c
}

// sameRow reports whether two rows are byte-for-byte equal, NULLs included.
func sameRow(a, b [][]byte) bool {
	for i := range a {
		if (a[i] == nil) != (b[i] == nil) || !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
