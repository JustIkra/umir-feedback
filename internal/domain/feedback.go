package domain

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Input struct {
	ID                                                   SubmissionID
	Name, Email, Phone, Message, ConsentVersion, Website string
	Accepted                                             bool
}

func Normalize(in Input) (Input, map[string]string) {
	in.ID = SubmissionID(strings.ToLower(string(in.ID)))
	in.Name = strings.TrimSpace(in.Name)
	in.Email = NormalizeEmail(in.Email)
	in.Phone = strings.TrimSpace(in.Phone)
	in.Message = strings.TrimSpace(in.Message)
	fields := make(map[string]string)
	textField(fields, "name", in.Name, 100, false)
	textField(fields, "message", in.Message, 2000, true)
	if in.Email == "" {
		fields["email"] = "Required"
	} else if !ValidEmail(in.Email) {
		fields["email"] = "InvalidFormat"
	}
	if in.Phone != "" {
		digits := 0
		valid := utf8.RuneCountInString(in.Phone) <= 32
		for _, r := range in.Phone {
			if r >= '0' && r <= '9' {
				digits++
			} else if !strings.ContainsRune(" +-()", r) {
				valid = false
			}
		}
		if !valid || digits < 10 || digits > 15 {
			fields["phone"] = "InvalidFormat"
		}
	}
	return in, fields
}

func textField(fields map[string]string, key, value string, max int, multiline bool) {
	if value == "" {
		fields[key] = "Required"
		return
	}
	if !utf8.ValidString(value) {
		fields[key] = "InvalidCharacters"
		return
	}
	if utf8.RuneCountInString(value) > max {
		fields[key] = "TooLong"
		return
	}
	for _, r := range value {
		allowedWhitespace := multiline && (r == '\n' || r == '\t')
		if unicode.IsControl(r) && !allowedWhitespace {
			fields[key] = "InvalidCharacters"
			return
		}
	}
}

func NormalizeEmail(value string) string {
	value = strings.TrimSpace(value)
	at := strings.LastIndexByte(value, '@')
	if at >= 0 {
		value = value[:at+1] + strings.ToLower(value[at+1:])
	}
	return value
}

func ValidEmail(value string) bool {
	if !utf8.ValidString(value) || len(value) > 254 || strings.Count(value, "@") != 1 {
		return false
	}
	parts := strings.SplitN(value, "@", 2)
	if parts[0] == "" || parts[1] == "" || !strings.Contains(parts[1], ".") {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("<>(),:;\\\"[]", r) {
			return false
		}
	}
	if strings.HasPrefix(parts[0], ".") || strings.HasSuffix(parts[0], ".") || strings.Contains(parts[0], "..") {
		return false
	}
	for _, r := range parts[0] {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune(".!#$%&'*+-/=?^_`{|}~", r) {
			return false
		}
	}
	for _, label := range strings.Split(parts[1], ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' {
				return false
			}
		}
	}
	return true
}

func ValidID(id SubmissionID) bool {
	s := string(id)
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' || s[14] != '4' || !strings.ContainsRune("89aAbB", rune(s[19])) {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		digit := r >= '0' && r <= '9'
		lowerHex := r >= 'a' && r <= 'f'
		upperHex := r >= 'A' && r <= 'F'
		if !digit && !lowerHex && !upperHex {
			return false
		}
	}
	return true
}
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 10, MaxAge: 24 * time.Hour, MinDelay: 10 * time.Second, MaxDelay: 15 * time.Minute}
}
func (p RetryPolicy) Valid() bool {
	return p.MaxAttempts > 0 && p.MaxAttempts <= 10 && p.MaxAge > 0 && p.MaxAge <= 24*time.Hour && p.MinDelay > 0 && p.MaxDelay >= p.MinDelay && p.MaxDelay <= 15*time.Minute
}
func (p RetryPolicy) Exhausted(attempts int, receivedAt, now time.Time) bool {
	return attempts >= p.MaxAttempts || !now.Before(receivedAt.Add(p.MaxAge))
}
func (p RetryPolicy) Delay(attempts int) time.Duration {
	delay := p.MinDelay
	for i := 1; i < attempts; i++ {
		if delay >= p.MaxDelay/2 {
			return p.MaxDelay
		}
		delay *= 2
	}
	if delay > p.MaxDelay {
		return p.MaxDelay
	}
	return delay
}
