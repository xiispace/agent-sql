package schemadiff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// parsedType is a column type split into the parts narrowing checks need.
type parsedType struct {
	base     string   // lower-case type name: "varchar", "int", "decimal"
	args     []int    // numeric arguments: length, or precision and scale
	members  []string // ENUM/SET members, unquoted
	unsigned bool
}

// BaseType returns the lower-case type name of a full column type
// ("varchar(64)" → "varchar", "int unsigned" → "int").
func BaseType(full string) string {
	return parseType(full).base
}

// pgAliases maps PostgreSQL's format_type spellings onto the names the
// narrowing rules use. Only the comparison sees these; rendered DDL keeps
// the server's own spelling.
var pgAliases = []struct{ from, to string }{
	{"character varying", "varchar"},
	{"bit varying", "varbit"},
	{"character", "char"},
	{"double precision", "double"},
	{"integer", "int"},
	{"int4", "int"},
	{"int8", "bigint"},
	{"int2", "smallint"},
	{"float8", "double"},
	{"float4", "float"},
	{"real", "float"},
	{"numeric", "decimal"},
}

var pgTimeZone = regexp.MustCompile(`^(timestamp|time)(\(\d+\))? with(out)? time zone`)

// canonicalType rewrites PostgreSQL type names to the MySQL-style names
// parseType understands. PostgreSQL's temporal types default to 6
// fractional digits, so an unqualified one is made explicit.
func canonicalType(s string) string {
	for _, a := range pgAliases {
		if strings.HasPrefix(s, a.from) {
			s = a.to + s[len(a.from):]
			break
		}
	}
	if m := pgTimeZone.FindStringSubmatch(s); m != nil {
		base := m[1]
		if m[3] == "" { // "with time zone"
			base += "tz"
		}
		prec := m[2]
		if prec == "" {
			prec = "(6)"
		}
		s = base + prec + s[len(m[0]):]
	}
	return s
}

func parseType(full string) parsedType {
	s := canonicalType(strings.ToLower(strings.TrimSpace(full)))
	var p parsedType
	name := s
	rest := ""
	if i := strings.IndexAny(s, "( "); i >= 0 {
		name, rest = s[:i], s[i:]
	}
	p.base = name
	if strings.HasPrefix(rest, "(") {
		end := matchingParen(rest)
		inner := rest[1:end]
		rest = rest[end+1:]
		if name == "enum" || name == "set" {
			p.members = splitMembers(inner)
		} else {
			for _, a := range strings.Split(inner, ",") {
				if n, err := strconv.Atoi(strings.TrimSpace(a)); err == nil {
					p.args = append(p.args, n)
				}
			}
		}
	}
	p.unsigned = strings.Contains(rest, "unsigned")
	return p
}

// matchingParen returns the index of the ')' closing the '(' at s[0],
// skipping parentheses inside quoted ENUM members.
func matchingParen(s string) int {
	depth := 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case inQuote:
			if ch == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
				} else {
					inQuote = false
				}
			}
		case ch == '\'':
			inQuote = true
		case ch == '(':
			depth++
		case ch == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(s) - 1
}

func splitMembers(inner string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		switch {
		case inQuote && ch == '\'':
			if i+1 < len(inner) && inner[i+1] == '\'' {
				cur.WriteByte('\'')
				i++
			} else {
				inQuote = false
				out = append(out, cur.String())
				cur.Reset()
			}
		case inQuote:
			cur.WriteByte(ch)
		case ch == '\'':
			inQuote = true
		}
	}
	return out
}

var intRank = map[string]int{"tinyint": 1, "smallint": 2, "mediumint": 3, "int": 4, "integer": 4, "bigint": 5}

var textRank = map[string]int{"tinytext": 1, "text": 2, "mediumtext": 3, "longtext": 4}

var blobRank = map[string]int{"tinyblob": 1, "blob": 2, "mediumblob": 3, "longblob": 4}

var floatRank = map[string]int{"float": 1, "double": 2, "real": 2}

var temporal = map[string]bool{"datetime": true, "timestamp": true, "time": true, "timestamptz": true, "timetz": true}

// typeChangeLoss reports whether changing a column from type `from` (the
// target's current type) to `to` (the source's type) can lose data, with a
// human-readable reason. Unrecognised type changes count as lossy: the
// classification errs towards asking for --allow-destructive.
func typeChangeLoss(from, to string) (bool, string) {
	if strings.EqualFold(strings.TrimSpace(from), strings.TrimSpace(to)) {
		return false, ""
	}
	f, t := parseType(from), parseType(to)
	narrowing := fmt.Sprintf("narrowing %s to %s can truncate data", from, to)

	if f.unsigned != t.unsigned && isNumeric(f.base) && isNumeric(t.base) {
		if f.unsigned {
			return true, fmt.Sprintf("removing UNSIGNED (%s to %s) can overflow large values", from, to)
		}
		return true, fmt.Sprintf("adding UNSIGNED (%s to %s) can reject negative values", from, to)
	}

	switch {
	case f.base == t.base:
		return sameBaseLoss(f, t, from, to, narrowing)
	case intRank[f.base] > 0 && intRank[t.base] > 0:
		return intRank[t.base] < intRank[f.base], narrowingIf(intRank[t.base] < intRank[f.base], narrowing)
	case textRank[f.base] > 0 && textRank[t.base] > 0:
		return textRank[t.base] < textRank[f.base], narrowingIf(textRank[t.base] < textRank[f.base], narrowing)
	case blobRank[f.base] > 0 && blobRank[t.base] > 0:
		return blobRank[t.base] < blobRank[f.base], narrowingIf(blobRank[t.base] < blobRank[f.base], narrowing)
	case floatRank[f.base] > 0 && floatRank[t.base] > 0:
		return floatRank[t.base] < floatRank[f.base], narrowingIf(floatRank[t.base] < floatRank[f.base], narrowing)
	case isCharLike(f.base) && textRank[t.base] > 0:
		return false, "" // varchar → text widens
	case isBinaryLike(f.base) && blobRank[t.base] > 0:
		return false, ""
	case f.base == "char" && t.base == "varchar" && arg(t, 0) >= arg(f, 0):
		return false, ""
	case f.base == "binary" && t.base == "varbinary" && arg(t, 0) >= arg(f, 0):
		return false, ""
	case textRank[f.base] > 0 && isCharLike(t.base):
		return true, fmt.Sprintf("converting %s to %s can truncate data", from, to)
	}
	return true, fmt.Sprintf("changing type %s to %s can convert or truncate data", from, to)
}

func narrowingIf(cond bool, reason string) string {
	if cond {
		return reason
	}
	return ""
}

func sameBaseLoss(f, t parsedType, from, to, narrowing string) (bool, string) {
	switch {
	case f.base == "enum" || f.base == "set":
		have := make(map[string]bool, len(t.members))
		for _, m := range t.members {
			have[m] = true
		}
		var removed []string
		for _, m := range f.members {
			if !have[m] {
				removed = append(removed, m)
			}
		}
		if len(removed) > 0 {
			return true, fmt.Sprintf("removing %s members %s can invalidate stored values", strings.ToUpper(f.base), strings.Join(removed, ", "))
		}
		return false, ""
	case f.base == "decimal" || f.base == "numeric":
		fp, fs := argOr(f, 0, 10), argOr(f, 1, 0)
		tp, ts := argOr(t, 0, 10), argOr(t, 1, 0)
		if ts < fs || tp-ts < fp-fs {
			return true, narrowing
		}
		return false, ""
	case temporal[f.base]:
		if argOr(t, 0, 0) < argOr(f, 0, 0) {
			return true, fmt.Sprintf("reducing fractional seconds (%s to %s) truncates values", from, to)
		}
		return false, ""
	case intRank[f.base] > 0:
		return false, "" // display width only
	case len(f.args) > 0 && len(t.args) > 0:
		if t.args[0] < f.args[0] {
			return true, narrowing
		}
		return false, ""
	}
	return false, ""
}

func arg(p parsedType, i int) int { return argOr(p, i, 0) }

func argOr(p parsedType, i, def int) int {
	if i < len(p.args) {
		return p.args[i]
	}
	return def
}

func isNumeric(base string) bool {
	return intRank[base] > 0 || floatRank[base] > 0 || base == "decimal" || base == "numeric"
}

func isCharLike(base string) bool { return base == "char" || base == "varchar" }

func isBinaryLike(base string) bool { return base == "binary" || base == "varbinary" }

// KeyKind classifies a key column for merge-joining two key-ordered row
// streams: how the driver orders it and how the rows' raw values compare.
type KeyKind int

const (
	// KeyBytes compares raw values byte by byte; drivers order such columns
	// by their binary representation.
	KeyBytes KeyKind = iota
	// KeyInteger compares values as integers.
	KeyInteger
	// KeyDecimal compares values as exact decimals.
	KeyDecimal
	// KeyFloat compares values as floating-point numbers.
	KeyFloat
)

// KeyKindOf returns the merge-join kind of a key column. Temporal values
// compare as bytes: their text forms are fixed-width and sort chronologically.
func KeyKindOf(c *Column) KeyKind {
	base := BaseType(c.Type)
	switch {
	case intRank[base] > 0 || base == "year":
		return KeyInteger
	case base == "decimal" || base == "numeric":
		return KeyDecimal
	case floatRank[base] > 0:
		return KeyFloat
	}
	return KeyBytes
}
