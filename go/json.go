package main

import (
	"math"
	"strconv"
	"strings"
)

// Ordered JSON values with a writer that reproduces V8's JSON.stringify byte
// for byte for the data this forge serves: object keys in insertion order,
// JS string escaping, JS number formatting. Every surface (REST, GraphQL,
// MCP) serializes through here so the bytes match the reference shapes.

type JVal interface{ jval() }

type JObj struct{ Fields []JField }
type JField struct {
	Key string
	Val JVal
}
type JArr struct{ Items []JVal }
type JStr string
type JInt int64
type JFloat float64
type JBool bool
type JNull struct{}

func (JObj) jval()   {}
func (JArr) jval()   {}
func (JStr) jval()   {}
func (JInt) jval()   {}
func (JFloat) jval() {}
func (JBool) jval()  {}
func (JNull) jval()  {}

var JNULL = JNull{}

func obj(pairs ...any) JObj {
	o := JObj{}
	for i := 0; i+1 < len(pairs); i += 2 {
		key, _ := pairs[i].(string)
		o.Fields = append(o.Fields, JField{Key: key, Val: toJVal(pairs[i+1])})
	}
	return o
}

func arr(items ...JVal) JArr { return JArr{Items: items} }

func toJVal(v any) JVal {
	switch t := v.(type) {
	case JVal:
		return t
	case nil:
		return JNULL
	case string:
		return JStr(t)
	case int:
		return JInt(int64(t))
	case int64:
		return t2(t)
	case bool:
		return JBool(t)
	case float64:
		return JFloat(t)
	default:
		return JNULL
	}
}

func t2(v int64) JVal { return JInt(v) }

// JS Number -> string for the values in play (integers and simple fractions).
func jsFormatNumber(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func writeJSONString(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		default:
			if r < 0x20 {
				sb.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				sb.WriteByte(hex[(r>>4)&0xf])
				sb.WriteByte(hex[r&0xf])
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
}

func writeJVal(sb *strings.Builder, v JVal, indent int, depth int) {
	switch t := v.(type) {
	case JNull:
		sb.WriteString("null")
	case JBool:
		if t {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case JStr:
		writeJSONString(sb, string(t))
	case JInt:
		sb.WriteString(strconv.FormatInt(int64(t), 10))
	case JFloat:
		sb.WriteString(jsFormatNumber(float64(t)))
	case JArr:
		if len(t.Items) == 0 {
			sb.WriteString("[]")
			return
		}
		if indent == 0 {
			sb.WriteByte('[')
			for i, item := range t.Items {
				if i > 0 {
					sb.WriteByte(',')
				}
				writeJVal(sb, item, indent, depth)
			}
			sb.WriteByte(']')
			return
		}
		sb.WriteString("[\n")
		pad := strings.Repeat(" ", indent*(depth+1))
		for i, item := range t.Items {
			if i > 0 {
				sb.WriteString(",\n")
			}
			sb.WriteString(pad)
			writeJVal(sb, item, indent, depth+1)
		}
		sb.WriteByte('\n')
		sb.WriteString(strings.Repeat(" ", indent*depth))
		sb.WriteByte(']')
	case JObj:
		if len(t.Fields) == 0 {
			sb.WriteString("{}")
			return
		}
		if indent == 0 {
			sb.WriteByte('{')
			for i, f := range t.Fields {
				if i > 0 {
					sb.WriteByte(',')
				}
				writeJSONString(sb, f.Key)
				sb.WriteByte(':')
				writeJVal(sb, f.Val, indent, depth)
			}
			sb.WriteByte('}')
			return
		}
		sb.WriteString("{\n")
		pad := strings.Repeat(" ", indent*(depth+1))
		for i, f := range t.Fields {
			if i > 0 {
				sb.WriteString(",\n")
			}
			sb.WriteString(pad)
			writeJSONString(sb, f.Key)
			sb.WriteString(": ")
			writeJVal(sb, f.Val, indent, depth+1)
		}
		sb.WriteByte('\n')
		sb.WriteString(strings.Repeat(" ", indent*depth))
		sb.WriteByte('}')
	}
}

// stringifyJSON is JSON.stringify(value): compact, insertion-ordered.
func stringifyJSON(v JVal) string {
	var sb strings.Builder
	writeJVal(&sb, v, 0, 0)
	return sb.String()
}

// stringifyJSONPretty is JSON.stringify(value, null, 2), as MCP tool text uses.
func stringifyJSONPretty(v JVal) string {
	var sb strings.Builder
	writeJVal(&sb, v, 2, 0)
	return sb.String()
}

// --- Decoding of request JSON (values as the TS code sees them after JSON.parse) ---

// A parsed JSON value in JS terms: map (insertion-ordered keys are not needed
// for inputs), []any, string, float64, bool, nil.
func jsGet(m map[string]any, key string) (any, bool) {
	v, ok := m[key]
	return v, ok
}

func jsStr(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func jsNum(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	}
	return 0, false
}
