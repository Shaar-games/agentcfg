package upstream

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Keep JSON-RPC number and string identities separate, but ignore equivalent
// string escaping and decimal notation. Never round numeric IDs through float64.
func requestIDKey(raw json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return "invalid:" + string(raw)
	}
	switch value := value.(type) {
	case string:
		return "string:" + value
	case json.Number:
		return numberIDKey(value.String())
	default:
		return "invalid:" + string(raw)
	}
}

func numberIDKey(number string) string {
	fallback := "number:" + number
	sign := ""
	if strings.HasPrefix(number, "-") {
		sign, number = "-", number[1:]
	}
	mantissa, exponentText, hasExponent := strings.Cut(strings.ToLower(number), "e")
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return "number:0"
	}
	var exponent int64
	if hasExponent {
		var err error
		exponent, err = strconv.ParseInt(exponentText, 10, 64)
		if err != nil {
			return fallback
		}
	}
	if exponent < math.MinInt64+int64(len(fraction)) {
		return fallback
	}
	exponent -= int64(len(fraction))
	trimmed := strings.TrimRight(digits, "0")
	trailing := int64(len(digits) - len(trimmed))
	if exponent > math.MaxInt64-trailing {
		return fallback
	}
	exponent += trailing
	return "number:" + sign + trimmed + "e" + strconv.FormatInt(exponent, 10)
}
