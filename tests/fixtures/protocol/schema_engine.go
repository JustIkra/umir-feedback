package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaInputLimit = 1 << 20
const schemaValidationLimit = 500 * time.Millisecond
const schemaMatchLimit = 4096

type matchBudget struct {
	deadline  time.Time
	remaining int
}

func (b *matchBudget) reset(duration time.Duration) {
	b.deadline = time.Now().Add(duration)
	b.remaining = schemaMatchLimit
}

func (b *matchBudget) check() {
	if !time.Now().Before(b.deadline) {
		panic(schemaMatchFailure{errors.New("fixture schema validation deadline")})
	}
}

type schemaMatchFailure struct{ cause error }

func (e schemaMatchFailure) Error() string { return "fixture schema match failed: " + e.cause.Error() }
func (e schemaMatchFailure) Unwrap() error { return e.cause }

func recoverSchemaMatch(err *error) {
	if value := recover(); value != nil {
		failure, ok := value.(schemaMatchFailure)
		if !ok {
			panic(value)
		}
		*err = failure
	}
}

type fixtureRegexp struct {
	regexp *regexp2.Regexp
	budget *matchBudget
}

func (r *fixtureRegexp) String() string { return r.regexp.String() }

func (r *fixtureRegexp) MatchString(input string) bool {
	r.budget.check()
	if strings.ContainsAny(input, "\u2028\u2029") && wildcardDot(r.String()) {
		// The pinned ECMAScript engine's dot class omits these line terminators.
		panic(schemaMatchFailure{errors.New("fixture ECMAScript match cannot be established")})
	}
	r.budget.remaining--
	if r.budget.remaining < 0 || len(input) > schemaInputLimit {
		panic(schemaMatchFailure{errors.New("fixture schema matching work bound")})
	}
	matched, err := r.regexp.MatchString(input)
	if err != nil {
		// A false result can satisfy not/anyOf. Abort the enclosing validation instead.
		panic(schemaMatchFailure{err})
	}
	r.budget.check()
	return matched
}

func wildcardDot(pattern string) bool {
	escaped, class := false, false
	for _, ch := range pattern {
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '[' {
			class = true
		}
		if ch == ']' {
			class = false
		}
		if ch == '.' && !class {
			return true
		}
	}
	return false
}

func (b *matchBudget) compile(pattern string) (jsonschema.Regexp, error) {
	b.check()
	re, err := regexp2.Compile(pattern, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	re.MatchTimeout = 100 * time.Millisecond
	return &fixtureRegexp{regexp: re, budget: b}, nil
}

type schemaValidator struct {
	schemas map[string]*jsonschema.Schema
	budget  matchBudget
}

func compileFixtureSchemas(data []byte) (validator *schemaValidator, err error) {
	defer recoverSchemaMatch(&err)
	validator = &schemaValidator{schemas: map[string]*jsonschema.Schema{}}
	validator.budget.reset(2 * time.Second)
	resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseRegexpEngine(validator.budget.compile)
	compiler.AssertFormat()
	if err := compiler.AddResource("fixture.json", resource); err != nil {
		return nil, err
	}
	var definitions struct {
		Definitions map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(data, &definitions); err != nil {
		return nil, err
	}
	keys := []string{"OwnerConfig", "Error"}
	for name := range definitions.Definitions {
		if name != "OwnerConfig" && name != "Error" {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		schema, err := compiler.Compile("fixture.json#/$defs/" + key)
		if err != nil {
			return nil, fmt.Errorf("compile fixture schema %s: %w", key, err)
		}
		validator.schemas[key] = schema
	}
	validator.budget.check()
	return validator, nil
}

func (v *schemaValidator) validate(name string, value any) (err error) {
	defer recoverSchemaMatch(&err)
	schema, ok := v.schemas[name]
	if !ok {
		return errors.New("unsupported schema")
	}
	v.budget.reset(schemaValidationLimit)
	err = schema.Validate(value)
	v.budget.check()
	return err
}
