package protocol

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/JustIkra/umir-feedback/internal/adapters/strictjson"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const Protocol = "umir-operational-fixtures/1.1.0"
const ControlPath = "/fixture/v1/control"
const ConfigPath = "/run/umir-fixtures/config/config.json"
const MaxRequest = 16384
const MaxResponse = 262144

//go:embed protocol.schema.json
var schemaJSON []byte
var schemasOnce sync.Once
var schemas map[string]*jsonschema.Schema
var schemaError error
var schemaValidators sync.Pool

func checkDocument(body []byte) error {
	if err := strictjson.CheckText(body); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := walkJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
func validate(name string, body []byte) error {
	if len(body) > schemaInputLimit {
		return errors.New("schema input bound")
	}
	if err := strictjson.CheckText(body); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := walkJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	schemasOnce.Do(func() {
		validator, err := compileFixtureSchemas(schemaJSON)
		if err != nil {
			schemaError = err
			return
		}
		schemas = validator.schemas
		schemaValidators.Put(validator)
	})
	if schemaError != nil {
		return schemaError
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return err
	}
	var validator *schemaValidator
	if pooled := schemaValidators.Get(); pooled != nil {
		var ok bool
		validator, ok = pooled.(*schemaValidator)
		if !ok {
			return errors.New("invalid fixture schema validator")
		}
	} else {
		validator, err = compileFixtureSchemas(schemaJSON)
		if err != nil {
			return err
		}
	}
	defer schemaValidators.Put(validator)
	return validator.validate(name, value)
}
func walkJSON(d *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("JSON depth")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	keys := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return errors.New("duplicate JSON key")
			}
			keys[name] = true
		}
		if err := walkJSON(d, depth+1); err != nil {
			return err
		}
	}
	closing, err := d.Token()
	if err != nil {
		return err
	}
	if delim == '{' && closing != json.Delim('}') || delim == '[' && closing != json.Delim(']') {
		return fmt.Errorf("invalid JSON delimiter")
	}
	return nil
}
