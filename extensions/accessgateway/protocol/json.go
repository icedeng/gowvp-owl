package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// DecodeStrict 同时拒绝重复键、未知字段、缺失字段和尾随数据。
// 标有 omitempty 的字段才允许省略，指针字段允许显式 null。
func DecodeStrict(data []byte, dst any) error {
	if !utf8.Valid(data) || !validEscapes(data) {
		return fmt.Errorf("JSON must contain valid Unicode")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readValue(decoder, 0)
	if err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	typ := reflect.TypeOf(dst)
	if typ == nil || typ.Kind() != reflect.Pointer {
		return fmt.Errorf("destination must be a pointer")
	}
	if err := checkShape(value, typ.Elem(), ""); err != nil {
		return err
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	return decoder.Decode(dst)
}

func readValue(d *json.Decoder, depth int) (any, error) {
	if depth > 48 {
		return nil, fmt.Errorf("JSON nesting limit")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		m := map[string]any{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("invalid object key")
			}
			if _, exists := m[key]; exists {
				return nil, fmt.Errorf("duplicate field %q", key)
			}
			value, err := readValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			m[key] = value
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("invalid object")
		}
		return m, nil
	case '[':
		a := []any{}
		for d.More() {
			v, err := readValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("invalid array")
		}
		return a, nil
	}
	return nil, fmt.Errorf("unexpected delimiter")
}

var rawType = reflect.TypeOf(json.RawMessage{})

func checkShape(value any, typ reflect.Type, path string) error {
	if typ == rawType || typ.Kind() == reflect.Interface {
		return nil
	}
	if typ.Kind() == reflect.Pointer {
		if value == nil {
			return nil
		}
		return checkShape(value, typ.Elem(), path)
	}
	if value == nil {
		return fmt.Errorf("%s cannot be null", path)
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be object", path)
		}
		fields := map[string]reflect.StructField{}
		var collect func(reflect.Type)
		collect = func(t reflect.Type) {
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if !f.IsExported() {
					continue
				}
				tag := f.Tag.Get("json")
				if tag == "-" {
					continue
				}
				if f.Anonymous && tag == "" {
					collect(f.Type)
					continue
				}
				name := strings.Split(tag, ",")[0]
				if name == "" {
					name = f.Name
				}
				fields[name] = f
			}
		}
		collect(typ)
		for name, f := range fields {
			v, exists := object[name]
			if !exists {
				if !strings.Contains(f.Tag.Get("json"), ",omitempty") {
					return fmt.Errorf("missing field %s%s", path, name)
				}
				continue
			}
			if err := checkShape(v, f.Type, path+name+"."); err != nil {
				return err
			}
		}
		for name := range object {
			if _, exists := fields[name]; !exists {
				return fmt.Errorf("unknown field %s%s", path, name)
			}
		}
	case reflect.Slice, reflect.Array:
		// []byte 的 JSON 表示为 base64 字符串。
		if typ.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be array", path)
		}
		for _, v := range array {
			if err := checkShape(v, typ.Elem(), path); err != nil {
				return err
			}
		}
	}
	return nil
}

// encoding/json 会容忍未配对的 UTF-16 surrogate；线协议与 JCS 明确拒绝。
func validEscapes(data []byte) bool {
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		n, ok := hex4(data[i+1 : i+5])
		if !ok {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, ok := hex4(data[i+3 : i+7])
			if !ok || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}
func hex4(b []byte) (uint16, bool) {
	var v uint16
	for _, c := range b {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v += uint16(c - '0')
		case c >= 'a' && c <= 'f':
			v += uint16(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			v += uint16(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return v, true
}
