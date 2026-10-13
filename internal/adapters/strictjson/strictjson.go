package strictjson

import (
	"errors"
	"strconv"
	"unicode/utf8"
)

var ErrText = errors.New("JSON text has invalid UTF-8 or an unpaired surrogate escape")

func CheckText(data []byte) error {
	if !utf8.Valid(data) {
		return ErrText
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) || data[i] != 'u' {
			continue
		}
		r, ok := hex4(data, i+1)
		if !ok {
			continue
		}
		i += 4
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			if i+6 < len(data) && data[i+1] == '\\' && data[i+2] == 'u' {
				if low, ok := hex4(data, i+3); ok && low >= 0xDC00 && low <= 0xDFFF {
					i += 6
					continue
				}
			}
			return ErrText
		case r >= 0xDC00 && r <= 0xDFFF:
			return ErrText
		}
	}
	return nil
}

func hex4(data []byte, at int) (rune, bool) {
	if at+4 > len(data) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(data[at:at+4]), 16, 16)
	return rune(n), err == nil
}
