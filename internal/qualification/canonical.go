package qualification

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CanonicalJSON v1 emits valid Unicode as UTF-8, escapes quotes, backslashes,
// and control characters (using lowercase hex for remaining U+0000..U+001F),
// does not HTML-escape or slash-escape, sorts object keys by UTF-8 bytes,
// removes insignificant whitespace, and preserves array order. Integer tokens
// are base-10 integers. Decimal/exponent tokens are interpreted as binary64
// and rendered with the shortest round-tripping significand: fixed notation
// for normalized exponents -6 through 20, scientific notation otherwise,
// lowercase `e`, explicit `+` on nonnegative scientific exponents, and no
// exponent zero padding. Any signed zero becomes `0`. The release checker
// recomputes only the documented string/integer/bool input-closure and
// identity payloads; it treats the controller-owned effective-config digest
// as opaque and does not attempt to reproduce this encoder over City values.
// Invalid UTF-8 is rejected instead of being silently replaced with U+FFFD.
func CanonicalJSON(value any) ([]byte, error) {
	if err := validateUTF8(reflect.ValueOf(value), 0); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, fmt.Errorf("decode normalized JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("check normalized JSON tail: %w", err)
	}
	var output bytes.Buffer
	if err := writeCanonicalJSON(&output, normalized); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func writeCanonicalJSON(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		output.WriteString(strconv.FormatBool(typed))
	case string:
		writeJSONString(output, typed)
	case json.Number:
		formatted, err := canonicalNumber(typed.String())
		if err != nil {
			return err
		}
		output.WriteString(formatted)
	case []any:
		output.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				output.WriteByte(',')
			}
			if err := writeCanonicalJSON(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if !utf8.ValidString(key) {
				return errors.New("canonical JSON object key is not valid UTF-8")
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				output.WriteByte(',')
			}
			writeJSONString(output, key)
			output.WriteByte(':')
			if err := writeCanonicalJSON(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("unsupported normalized JSON value %T", value)
	}
	return nil
}

func canonicalNumber(raw string) (string, error) {
	if raw == "-0" {
		return "0", nil
	}
	if !strings.ContainsAny(raw, ".eE") {
		return raw, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return "", fmt.Errorf("parse canonical JSON number %q: %w", raw, err)
	}
	if value == 0 {
		mantissa := raw
		if exponent := strings.IndexAny(mantissa, "eE"); exponent >= 0 {
			mantissa = mantissa[:exponent]
		}
		for _, digit := range mantissa {
			if digit >= '1' && digit <= '9' {
				return "", fmt.Errorf("canonical JSON number %q underflows binary64", raw)
			}
		}
		return "0", nil
	}
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	parts := strings.SplitN(scientific, "e", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("format canonical JSON number %q: missing exponent", raw)
	}
	exponent, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", fmt.Errorf("format canonical JSON number %q: %w", raw, err)
	}
	mantissa := parts[0]
	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign = "-"
		mantissa = strings.TrimPrefix(mantissa, "-")
	}
	digits := strings.ReplaceAll(mantissa, ".", "")
	if exponent >= -6 && exponent < 21 {
		decimalPosition := exponent + 1
		switch {
		case decimalPosition <= 0:
			return sign + "0." + strings.Repeat("0", -decimalPosition) + digits, nil
		case decimalPosition >= len(digits):
			return sign + digits + strings.Repeat("0", decimalPosition-len(digits)), nil
		default:
			return sign + digits[:decimalPosition] + "." + digits[decimalPosition:], nil
		}
	}
	positiveExponentSign := ""
	if exponent >= 0 {
		positiveExponentSign = "+"
	}
	if len(digits) == 1 {
		return sign + digits + "e" + positiveExponentSign + strconv.Itoa(exponent), nil
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + positiveExponentSign + strconv.Itoa(exponent), nil
}

func writeJSONString(output *bytes.Buffer, value string) {
	output.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"':
			output.WriteString(`\"`)
		case '\\':
			output.WriteString(`\\`)
		case '\b':
			output.WriteString(`\b`)
		case '\f':
			output.WriteString(`\f`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		case '\t':
			output.WriteString(`\t`)
		default:
			if character < 0x20 {
				output.WriteString(`\u00`)
				output.WriteByte(lowerHexDigit(byte(character >> 4)))
				output.WriteByte(lowerHexDigit(byte(character & 0x0f)))
			} else {
				output.WriteRune(character)
			}
		}
	}
	output.WriteByte('"')
}

func lowerHexDigit(value byte) byte {
	if value < 10 {
		return '0' + value
	}
	return 'a' + value - 10
}

func validateUTF8(value reflect.Value, depth int) error {
	if !value.IsValid() {
		return nil
	}
	if depth > 100 {
		return errors.New("canonical JSON value exceeds maximum nesting depth")
	}
	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return errors.New("canonical JSON string is not valid UTF-8")
		}
	case reflect.Interface, reflect.Pointer:
		if !value.IsNil() {
			return validateUTF8(value.Elem(), depth+1)
		}
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return nil
		}
		for i := 0; i < value.Len(); i++ {
			if err := validateUTF8(value.Index(i), depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateUTF8(iterator.Key(), depth+1); err != nil {
				return err
			}
			if err := validateUTF8(iterator.Value(), depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		typeOfValue := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := typeOfValue.Field(i)
			if field.PkgPath != "" || strings.Split(field.Tag.Get("json"), ",")[0] == "-" {
				continue
			}
			if err := validateUTF8(value.Field(i), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
