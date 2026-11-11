package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Canonical 按 RFC8785 的 UTF-16 属性顺序和 ECMAScript 数字表示生成 JCS。
func Canonical(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if !validEscapes(data) {
		return nil, invalid("Unicode")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	obj, err := readValue(d, 0)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := writeCanonical(&out, obj); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func Hash(v any) (string, error) {
	b, e := Canonical(v)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
func ConfigHash(c EffectiveConfig) (string, error) {
	if err := ValidateConfig(c); err != nil {
		return "", err
	}
	c.SourceCIDRs = slices.Clone(c.SourceCIDRs)
	sort.Strings(c.SourceCIDRs)
	return Hash(c)
}

// BusinessHash 在严格校验后调用，排除操作身份和临时票据，集合字段排序。
func BusinessHash(v any) (string, error) {
	data, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	obj, e := readValue(d, 0)
	if e != nil {
		return "", e
	}
	if e = normalizeBusiness(obj); e != nil {
		return "", e
	}
	return Hash(obj)
}
func normalizeBusiness(v any) error {
	switch obj := v.(type) {
	case map[string]any:
		delete(obj, "operation_id")
		delete(obj, "ticket")
		for k, v := range obj {
			if items, ok := v.([]any); ok && oneOf(k, "source_ips", "source_cidrs", "required_capabilities", "renew_session_ids", "renew_transfer_ids") {
				sorted := make([]string, len(items))
				seen := map[string]bool{}
				for i, item := range items {
					s, ok := item.(string)
					if !ok {
						return invalid(k)
					}
					if k == "source_ips" {
						ip, e := netip.ParseAddr(s)
						if e != nil {
							return e
						}
						s = ip.Unmap().String()
					}
					if k == "source_cidrs" {
						p, e := netip.ParsePrefix(s)
						if e != nil {
							return e
						}
						s = p.Masked().String()
					}
					if seen[s] {
						return invalid("duplicate " + k)
					}
					seen[s] = true
					sorted[i] = s
				}
				sort.Strings(sorted)
				obj[k] = sorted
			} else if e := normalizeBusiness(v); e != nil {
				return e
			}
		}
	case []any:
		for _, v := range obj {
			if e := normalizeBusiness(v); e != nil {
				return e
			}
		}
	}
	return nil
}
func writeCanonical(out *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if x {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		writeString(out, x)
	case json.Number:
		f, e := strconv.ParseFloat(string(x), 64)
		if e != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return invalid("JCS number")
		}
		out.WriteString(ecmaNumber(f))
	case []any:
		out.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				out.WriteByte(',')
			}
			if e := writeCanonical(out, item); e != nil {
				return e
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		out.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			writeString(out, k)
			out.WriteByte(':')
			if e := writeCanonical(out, x[k]); e != nil {
				return e
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical value %T", v)
	}
	return nil
}
func utf16Less(a, b string) bool {
	aa := utf16.Encode([]rune(a))
	bb := utf16.Encode([]rune(b))
	return slices.Compare(aa, bb) < 0
}
func writeString(out *bytes.Buffer, s string) {
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}
func ecmaNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	a := math.Abs(f)
	format := byte('f')
	if a < 1e-6 || a >= 1e21 {
		format = 'e'
	}
	s := strconv.FormatFloat(f, format, -1, 64)
	if i := strings.IndexByte(s, 'e'); i >= 0 {
		exp, _ := strconv.Atoi(s[i+1:])
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		return s[:i] + "e" + sign + strconv.Itoa(exp)
	}
	return s
}
