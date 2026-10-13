package bootstrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/strictjson"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

var exportInstant = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d{1,9})?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$`)

func validateExport(body []byte, tenant string, limit int) error {
	invalid := errors.New("invalid scoped export response")
	if strictjson.CheckText(body) != nil {
		return invalid
	}
	d := json.NewDecoder(bytes.NewReader(body))
	if exportValue(d, 0, "page") != nil {
		return invalid
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return invalid
	}
	var result struct {
		TenantID string `json:"tenantID"`
		Items    *[]*struct {
			Receipt *struct {
				SubmissionID string `json:"submissionID"`
				ReceivedAt   string `json:"receivedAt"`
			} `json:"receipt"`
			Scope *domain.Namespace `json:"scope"`
			State string            `json:"state"`
		} `json:"items"`
		NextCursor json.RawMessage `json:"nextCursor"`
	}
	d = json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&result) != nil || result.TenantID != tenant || result.Items == nil || len(*result.Items) > limit {
		return invalid
	}
	if len(result.NextCursor) != 0 {
		var cursor string
		if json.Unmarshal(result.NextCursor, &cursor) != nil || cursor == "" {
			return invalid
		}
	}
	for _, item := range *result.Items {
		if item == nil || item.Receipt == nil || item.Scope == nil || item.Scope.Kind != "app" || item.Scope.TenantID != tenant || !item.Scope.Valid() || (item.State != "accepted" && item.State != "duplicate") {
			return invalid
		}
		receipt := item.Receipt
		if receipt.SubmissionID != strings.ToLower(receipt.SubmissionID) || !domain.ValidID(domain.SubmissionID(receipt.SubmissionID)) || !exportInstant.MatchString(receipt.ReceivedAt) {
			return invalid
		}
		if _, err := time.Parse(time.RFC3339Nano, receipt.ReceivedAt); err != nil {
			return invalid
		}
	}
	return nil
}

func exportValue(d *json.Decoder, depth int, shape string) error {
	if depth > 8 {
		return errors.New("export nesting bound")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		child := "item"
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] || !exportKey(shape, name) {
				return errors.New("invalid export key")
			}
			seen[name] = true
			child = name
		}
		if err := exportValue(d, depth+1, child); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func exportKey(shape, key string) bool {
	switch shape {
	case "page":
		return key == "tenantID" || key == "items" || key == "nextCursor"
	case "item":
		return key == "receipt" || key == "scope" || key == "state"
	case "receipt":
		return key == "submissionID" || key == "receivedAt"
	case "scope":
		return key == "kind" || key == "userID" || key == "orgID" || key == "tenantID"
	default:
		return false
	}
}
