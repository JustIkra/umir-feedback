package domain

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeAndValidate(t *testing.T) {
	in := Input{ID: "123e4567-e89b-42d3-a456-426614174000", Name: "  Оператор  ", Email: " User@EXAMPLE.ORG ", Phone: " +7 (999) 123-45-67 ", Message: "  первая\nвторая\tстрока  ", Accepted: true, ConsentVersion: "v1"}
	got, fields := Normalize(in)
	if len(fields) != 0 || got.Name != "Оператор" || got.Email != "User@example.org" || got.Message != "первая\nвторая\tстрока" {
		t.Fatalf("normalization: %#v %#v", got, fields)
	}
	for _, tc := range []struct{ field, value, code string }{
		{"name", "\x00bad", "InvalidCharacters"}, {"name", strings.Repeat("я", 101), "TooLong"},
		{"email", "x\n@y.org", "InvalidFormat"}, {"email", "x@@y.org", "InvalidFormat"}, {"email", "a<@b.org", "InvalidFormat"},
		{"phone", "+123", "InvalidFormat"}, {"message", "a\rb", "InvalidCharacters"}, {"message", " ", "Required"},
	} {
		t.Run(tc.field+tc.code+tc.value, func(t *testing.T) {
			next := in
			switch tc.field {
			case "name":
				next.Name = tc.value
			case "email":
				next.Email = tc.value
			case "phone":
				next.Phone = tc.value
			case "message":
				next.Message = tc.value
			}
			_, errs := Normalize(next)
			if errs[tc.field] != tc.code {
				t.Fatalf("got %v", errs)
			}
		})
	}
}

func TestUUIDv4(t *testing.T) {
	for _, id := range []string{"123e4567-e89b-42d3-a456-426614174000", "123E4567-E89B-42D3-A456-426614174000"} {
		if !ValidID(SubmissionID(id)) {
			t.Fatal(id)
		}
	}
	for _, id := range []string{"123e4567-e89b-12d3-a456-426614174000", "123e4567-e89b-42d3-7456-426614174000", "bad"} {
		if ValidID(SubmissionID(id)) {
			t.Fatal(id)
		}
	}
}

func TestRetryPolicyBoundaries(t *testing.T) {
	p := DefaultRetryPolicy()
	at := time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	if !p.Valid() || p.Exhausted(9, at, at.Add(23*time.Hour)) || !p.Exhausted(10, at, at) || !p.Exhausted(1, at, at.Add(24*time.Hour)) {
		t.Fatal("retry boundary")
	}
	if p.Delay(1) != 10*time.Second || p.Delay(2) != 20*time.Second || p.Delay(10) != 15*time.Minute {
		t.Fatal("backoff")
	}
}
