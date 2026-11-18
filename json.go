// JSON value model, parser and encoder with Node.js JSON semantics:
// key order preserved, duplicate keys keep first position with last value,
// strings escape only ", \, and control chars (no <>& escaping, no U+2028
// escaping), numbers format like ECMAScript Number::toString.
package main

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

type JKind int

const (
	JNull JKind = iota
	JBool
	JNum
	JStr
	JArr
	JObj
	JUndef // JS undefined sentinel: omitted in objects, null in arrays
)

type JField struct {
	Key string
	Val *JVal
}

type JNumVal struct {
	IsInt bool
	I     int64
	F     float64
}

type JVal struct {
	Kind JKind
	Bool bool
	Num  JNumVal
	Str  string
	Arr  []*JVal
	Obj  []JField
}

func JNullV() *JVal             { return &JVal{Kind: JNull} }
func JUndefV() *JVal            { return &JVal{Kind: JUndef} }
func JBoolV(b bool) *JVal       { return &JVal{Kind: JBool, Bool: b} }
func JInt(i int64) *JVal        { return &JVal{Kind: JNum, Num: JNumVal{IsInt: true, I: i}} }
func JIntV(i int) *JVal         { return JInt(int64(i)) }
func JFloat(f float64) *JVal    { return &JVal{Kind: JNum, Num: JNumVal{F: f}} }
func JStrV(s string) *JVal      { return &JVal{Kind: JStr, Str: s} }
func JArrV(items ...*JVal) *JVal { return &JVal{Kind: JArr, Arr: items} }
func JObjV(fields ...JField) *JVal {
	return &JVal{Kind: JObj, Obj: fields}
}

func JF(key string, val *JVal) JField { return JField{Key: key, Val: val} }

// ObjGet returns the value for key, or nil when absent / not an object.
func (v *JVal) ObjGet(key string) *JVal {
	if v == nil || v.Kind != JObj {
		return nil
	}
	for i := range v.Obj {
		if v.Obj[i].Key == key {
			return v.Obj[i].Val
		}
	}
	return nil
}

// ObjSet sets key, keeping first position on duplicates (JS semantics).
func (v *JVal) ObjSet(key string, val *JVal) {
	if v.Kind != JObj {
		return
	}
	for i := range v.Obj {
		if v.Obj[i].Key == key {
			v.Obj[i].Val = val
			return
		}
	}
	v.Obj = append(v.Obj, JField{Key: key, Val: val})
}

func (v *JVal) IsStr() bool  { return v != nil && v.Kind == JStr }
func (v *JVal) IsNum() bool  { return v != nil && v.Kind == JNum }
func (v *JVal) IsBool() bool { return v != nil && v.Kind == JBool }
func (v *JVal) IsArr() bool  { return v != nil && v.Kind == JArr }
func (v *JVal) IsObj() bool  { return v != nil && v.Kind == JObj }

// jsStrOf implements JS String(v): strings as-is, numbers via Number::toString,
// booleans, null -> "null", undefined -> "undefined", arrays joined with ",",
// objects -> "[object Object]".
func jsStrOf(v *JVal) string {
	if v == nil {
		return "undefined"
	}
	switch v.Kind {
	case JStr:
		return v.Str
	case JNum:
		return jsNumString(v.Num)
	case JBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case JNull:
		return "null"
	case JUndef:
		return "undefined"
	case JArr:
		parts := make([]string, len(v.Arr))
		for i, item := range v.Arr {
			if item.Kind == JNull || item.Kind == JUndef {
				parts[i] = ""
			} else {
				parts[i] = jsStrOf(item)
			}
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// jsNumString formats a number like ECMAScript Number::toString.
func jsNumString(n JNumVal) string {
	if n.IsInt {
		return strconv.FormatInt(n.I, 10)
	}
	return jsFloatString(n.F)
}

// jsFloatString implements ECMAScript Number::toString for a float64.
func jsFloatString(f float64) string {
	if f != f {
		return "NaN"
	}
	if f > 1.7976931348623157e308 {
		return "Infinity"
	}
	if f < -1.7976931348623157e308 {
		return "-Infinity"
	}
	if f == 0 {
		return "0"
	}
	neg := false
	if f < 0 {
		neg = true
		f = -f
	}
	// Shortest round-trip digits in scientific form: "d.dddde±X".
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	// sci looks like "1.2345e+07" or "5e-07".
	ePos := strings.IndexByte(sci, 'e')
	mant := sci[:ePos]
	exp, _ := strconv.Atoi(sci[ePos+1:])
	digits := strings.Replace(mant, ".", "", 1)
	// Strip leading zeros (cannot happen for normalized shortest form except "0.x").
	digits = strings.TrimLeft(digits, "0")
	// Value = 0.digits * 10^n where n = exp+1.
	n := exp + 1
	k := len(digits)
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		expVal := n - 1
		expStr := strconv.Itoa(expVal)
		if expVal >= 0 {
			expStr = "+" + expStr
		}
		if k == 1 {
			out = digits + "e" + expStr
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + expStr
		}
	}
	if neg {
		return "-" + out
	}
	return out
}

// jsParseNumber implements JS Number(string): trims whitespace, "" -> 0,
// hex/octal/binary, Infinity, else decimal float; NaN on failure.
func jsParseNumber(s string) float64 {
	s = strings.Trim(s, " \t\n\v\f\r               　\uFEFF")
	if s == "" {
		return 0
	}
	if s == "Infinity" || s == "+Infinity" {
		return math.Inf(1) // +Inf
	}
	if s == "-Infinity" {
		return math.Inf(-1)
	}
	neg := false
	rest := s
	if len(rest) > 0 && (rest[0] == '+' || rest[0] == '-') {
		neg = rest[0] == '-'
		rest = rest[1:]
	}
	if len(rest) > 2 && rest[0] == '0' {
		switch rest[1] {
		case 'x', 'X':
			v, err := strconv.ParseInt(rest[2:], 16, 64)
			if err != nil {
				return nan()
			}
			if neg {
				return -float64(v)
			}
			return float64(v)
		case 'b', 'B':
			v, err := strconv.ParseInt(rest[2:], 2, 64)
			if err != nil {
				return nan()
			}
			if neg {
				return -float64(v)
			}
			return float64(v)
		case 'o', 'O':
			v, err := strconv.ParseInt(rest[2:], 8, 64)
			if err != nil {
				return nan()
			}
			if neg {
				return -float64(v)
			}
			return float64(v)
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nan()
	}
	return f
}

func nan() float64 {
	f, _ := strconv.ParseFloat("NaN", 64)
	return f
}

// jsToNumber implements JS Number(v) for a JSON value.
func jsToNumber(v *JVal) float64 {
	if v == nil {
		return nan()
	}
	switch v.Kind {
	case JNum:
		if v.Num.IsInt {
			return float64(v.Num.I)
		}
		return v.Num.F
	case JStr:
		return jsParseNumber(v.Str)
	case JBool:
		if v.Bool {
			return 1
		}
		return 0
	case JNull:
		return 0
	case JUndef:
		return nan()
	case JArr:
		if len(v.Arr) == 0 {
			return 0
		}
		if len(v.Arr) == 1 {
			return jsToNumber(v.Arr[0])
		}
		return nan()
	default:
		return nan()
	}
}

// jsToInt converts like Number(v) then checks integer-ness; returns ok=false
// when NaN, infinite, or non-integral.
func jsToInt(v *JVal) (int64, bool) {
	f := jsToNumber(v)
	if f != f || f > 9.007199254740992e15 || f < -9.007199254740992e15 {
		return 0, false
	}
	if f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

// ---------- Parser ----------

type jParser struct {
	b   []byte
	pos int
}

func parseJSON(b []byte) (*JVal, error) {
	p := &jParser{b: b}
	p.skipWs()
	v, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	p.skipWs()
	if p.pos != len(p.b) {
		return nil, errSyntax
	}
	return v, nil
}

var errSyntax = &jsonError{"unexpected token"}

type jsonError struct{ msg string }

func (e *jsonError) Error() string { return e.msg }

func (p *jParser) skipWs() {
	for p.pos < len(p.b) {
		c := p.b[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++
		} else {
			break
		}
	}
}

func (p *jParser) parseValue() (*JVal, error) {
	if p.pos >= len(p.b) {
		return nil, errSyntax
	}
	switch c := p.b[p.pos]; {
	case c == '{':
		return p.parseObject()
	case c == '[':
		return p.parseArray()
	case c == '"':
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return JStrV(s), nil
	case c == 't':
		if p.consume("true") {
			return JBoolV(true), nil
		}
		return nil, errSyntax
	case c == 'f':
		if p.consume("false") {
			return JBoolV(false), nil
		}
		return nil, errSyntax
	case c == 'n':
		if p.consume("null") {
			return JNullV(), nil
		}
		return nil, errSyntax
	default:
		return p.parseNumber()
	}
}

func (p *jParser) consume(s string) bool {
	if p.pos+len(s) <= len(p.b) && string(p.b[p.pos:p.pos+len(s)]) == s {
		p.pos += len(s)
		return true
	}
	return false
}

func (p *jParser) parseObject() (*JVal, error) {
	p.pos++ // {
	obj := JObjV()
	p.skipWs()
	if p.pos < len(p.b) && p.b[p.pos] == '}' {
		p.pos++
		return obj, nil
	}
	for {
		p.skipWs()
		if p.pos >= len(p.b) || p.b[p.pos] != '"' {
			return nil, errSyntax
		}
		key, err := p.parseString()
		if err != nil {
			return nil, err
		}
		p.skipWs()
		if p.pos >= len(p.b) || p.b[p.pos] != ':' {
			return nil, errSyntax
		}
		p.pos++
		p.skipWs()
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		obj.ObjSet(key, val)
		p.skipWs()
		if p.pos >= len(p.b) {
			return nil, errSyntax
		}
		if p.b[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.b[p.pos] == '}' {
			p.pos++
			return obj, nil
		}
		return nil, errSyntax
	}
}

func (p *jParser) parseArray() (*JVal, error) {
	p.pos++ // [
	arr := JArrV()
	p.skipWs()
	if p.pos < len(p.b) && p.b[p.pos] == ']' {
		p.pos++
		return arr, nil
	}
	for {
		p.skipWs()
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		arr.Arr = append(arr.Arr, val)
		p.skipWs()
		if p.pos >= len(p.b) {
			return nil, errSyntax
		}
		if p.b[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.b[p.pos] == ']' {
			p.pos++
			return arr, nil
		}
		return nil, errSyntax
	}
}

// parseString decodes a JSON string starting at the opening quote.
// Raw bytes >= 0x80 are decoded as UTF-8 with U+FFFD replacement (Node's
// Buffer.toString('utf8') behaviour); \u escapes combine surrogate pairs.
func (p *jParser) parseString() (string, error) {
	p.pos++ // "
	var sb strings.Builder
	for p.pos < len(p.b) {
		c := p.b[p.pos]
		if c == '"' {
			p.pos++
			return sb.String(), nil
		}
		if c < 0x20 {
			return "", errSyntax
		}
		if c == '\\' {
			p.pos++
			if p.pos >= len(p.b) {
				return "", errSyntax
			}
			esc := p.b[p.pos]
			p.pos++
			switch esc {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				r, err := p.parseUnicodeEscape()
				if err != nil {
					return "", err
				}
				sb.WriteRune(r)
			default:
				return "", errSyntax
			}
			continue
		}
		if c < 0x80 {
			sb.WriteByte(c)
			p.pos++
			continue
		}
		r, size := utf8.DecodeRune(p.b[p.pos:])
		sb.WriteRune(r)
		p.pos += size
	}
	return "", errSyntax
}

func (p *jParser) parseUnicodeEscape() (rune, error) {
	if p.pos+4 > len(p.b) {
		return 0, errSyntax
	}
	hex := string(p.b[p.pos : p.pos+4])
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return 0, errSyntax
	}
	p.pos += 4
	r := rune(v)
	if r >= 0xD800 && r <= 0xDBFF {
		// High surrogate: combine with a following \uDC00-\uDFFF escape.
		if p.pos+6 <= len(p.b) && p.b[p.pos] == '\\' && p.b[p.pos+1] == 'u' {
			hex2 := string(p.b[p.pos+2 : p.pos+6])
			v2, err := strconv.ParseUint(hex2, 16, 32)
			if err == nil {
				r2 := rune(v2)
				if r2 >= 0xDC00 && r2 <= 0xDFFF {
					p.pos += 6
					return 0x10000 + ((r - 0xD800) << 10) + (r2 - 0xDC00), nil
				}
			}
		}
		return utf8.RuneError, nil
	}
	if r >= 0xDC00 && r <= 0xDFFF {
		return utf8.RuneError, nil
	}
	return r, nil
}

func (p *jParser) parseNumber() (*JVal, error) {
	start := p.pos
	if p.pos < len(p.b) && p.b[p.pos] == '-' {
		p.pos++
	}
	if p.pos >= len(p.b) {
		return nil, errSyntax
	}
	if p.b[p.pos] == '0' {
		p.pos++
	} else if p.b[p.pos] >= '1' && p.b[p.pos] <= '9' {
		for p.pos < len(p.b) && p.b[p.pos] >= '0' && p.b[p.pos] <= '9' {
			p.pos++
		}
	} else {
		return nil, errSyntax
	}
	isFloat := false
	if p.pos < len(p.b) && p.b[p.pos] == '.' {
		isFloat = true
		p.pos++
		if p.pos >= len(p.b) || p.b[p.pos] < '0' || p.b[p.pos] > '9' {
			return nil, errSyntax
		}
		for p.pos < len(p.b) && p.b[p.pos] >= '0' && p.b[p.pos] <= '9' {
			p.pos++
		}
	}
	if p.pos < len(p.b) && (p.b[p.pos] == 'e' || p.b[p.pos] == 'E') {
		isFloat = true
		p.pos++
		if p.pos < len(p.b) && (p.b[p.pos] == '+' || p.b[p.pos] == '-') {
			p.pos++
		}
		if p.pos >= len(p.b) || p.b[p.pos] < '0' || p.b[p.pos] > '9' {
			return nil, errSyntax
		}
		for p.pos < len(p.b) && p.b[p.pos] >= '0' && p.b[p.pos] <= '9' {
			p.pos++
		}
	}
	lit := string(p.b[start:p.pos])
	if !isFloat {
		if i, err := strconv.ParseInt(lit, 10, 64); err == nil {
			return JInt(i), nil
		}
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return nil, errSyntax
	}
	return JFloat(f), nil
}

// ---------- Encoder (Node JSON.stringify semantics) ----------

func encodeJSON(v *JVal) []byte {
	var buf bytes.Buffer
	writeJSON(&buf, v)
	return buf.Bytes()
}

func writeJSON(buf *bytes.Buffer, v *JVal) {
	if v == nil {
		buf.WriteString("null")
		return
	}
	switch v.Kind {
	case JNull, JUndef:
		buf.WriteString("null")
	case JBool:
		if v.Bool {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case JNum:
		buf.WriteString(jsNumString(v.Num))
	case JStr:
		writeJSONString(buf, v.Str)
	case JArr:
		buf.WriteByte('[')
		for i, item := range v.Arr {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSON(buf, item)
		}
		buf.WriteByte(']')
	case JObj:
		buf.WriteByte('{')
		first := true
		for _, f := range v.Obj {
			if f.Val.Kind == JUndef {
				continue
			}
			if !first {
				buf.WriteByte(',')
			}
			first = false
			writeJSONString(buf, f.Key)
			buf.WriteByte(':')
			writeJSON(buf, f.Val)
		}
		buf.WriteByte('}')
	}
}

// writeJSONString escapes like JSON.stringify: " \ and control chars only.
func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '"':
			buf.WriteString("\\\"")
			i++
		case '\\':
			buf.WriteString("\\\\")
			i++
		case '\b':
			buf.WriteString("\\b")
			i++
		case '\f':
			buf.WriteString("\\f")
			i++
		case '\n':
			buf.WriteString("\\n")
			i++
		case '\r':
			buf.WriteString("\\r")
			i++
		case '\t':
			buf.WriteString("\\t")
			i++
		default:
			if c < 0x20 {
				const hexd = "0123456789abcdef"
				buf.WriteString("\\u00")
				buf.WriteByte(hexd[c>>4])
				buf.WriteByte(hexd[c&0xf])
				i++
			} else if c < 0x80 {
				buf.WriteByte(c)
				i++
			} else {
				r, size := utf8.DecodeRuneInString(s[i:])
				if r == utf8.RuneError && size == 1 {
					buf.WriteString("�")
				} else {
					buf.WriteString(s[i : i+size])
				}
				i += size
			}
		}
	}
	buf.WriteByte('"')
}
