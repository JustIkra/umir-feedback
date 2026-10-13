package protocol

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestFixtureSchemaRejectsDuplicateAndUnknownFields(t *testing.T) {
	for _, body := range []string{`{"version":1,"project":"umir-backend-000000000000000000000000","requestID":"11111111-1111-4111-8111-111111111111","operation":"clock.read","data":{},"secret":"not-allowed"}`, `{"version":1,"version":1,"project":"umir-backend-000000000000000000000000","requestID":"11111111-1111-4111-8111-111111111111","operation":"clock.read","data":{}}`} {
		if validate("clock_read_request", []byte(body)) == nil {
			t.Fatal("invalid fixture JSON accepted")
		}
	}
}
func TestApprovedSpecificationExamples(t *testing.T) {
	data, e := os.ReadFile("examples.json")
	if e != nil {
		t.Fatal(e)
	}
	var examples struct {
		Positive, Negative []struct {
			Schema string          `json:"schema"`
			Value  json.RawMessage `json:"value"`
		}
	}
	if e = json.Unmarshal(data, &examples); e != nil {
		t.Fatal(e)
	}
	for _, v := range examples.Positive {
		name := strings.TrimPrefix(v.Schema, "#/$defs/")
		if e = validate(name, v.Value); e != nil {
			t.Fatalf("approved positive %s: %v", name, e)
		}
	}
	for _, v := range examples.Negative {
		name := strings.TrimPrefix(v.Schema, "#/$defs/")
		if e = validate(name, v.Value); e == nil {
			t.Fatalf("approved negative accepted %s", name)
		}
	}
}
