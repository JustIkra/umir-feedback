package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type Submitter interface {
	Submit(context.Context, domain.Input) (domain.Receipt, bool, error)
}
type wireInput struct {
	SubmissionID domain.SubmissionID `json:"submissionID"`
	Name         string              `json:"name"`
	Email        string              `json:"email"`
	Phone        string              `json:"phone"`
	Message      string              `json:"message"`
	Consent      struct {
		Accepted        bool   `json:"accepted"`
		DocumentVersion string `json:"documentVersion"`
	} `json:"consent"`
	Website string `json:"website"`
}
type errorBody struct {
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	FieldErrors map[string]string `json:"fieldErrors,omitempty"`
	RequestID   string            `json:"requestID"`
}

func feedbackHandler(logger *slog.Logger, intake Submitter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		idBytes := make([]byte, 16)
		if _, err := rand.Read(idBytes); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			logger.Error("request identity unavailable")
			return
		}
		requestID := hex.EncodeToString(idBytes)
		respond := func(status int, code string, fields map[string]string) {
			writeJSON(w, status, errorBody{Code: code, Message: code, FieldErrors: fields, RequestID: requestID}, logger)
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			respond(405, "MethodNotAllowed", nil)
			return
		}
		media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
			respond(415, "UnsupportedMediaType", nil)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				respond(413, "PayloadTooLarge", nil)
			} else {
				respond(400, "InvalidRequest", nil)
			}
			return
		}
		in, err := decodeInput(body)
		if err != nil {
			respond(400, "InvalidRequest", nil)
			return
		}
		if intake == nil {
			respond(503, "FeedbackStorageUnavailable", nil)
			return
		}
		receipt, created, err := intake.Submit(r.Context(), domain.Input{ID: in.SubmissionID, Name: in.Name, Email: in.Email, Phone: in.Phone, Message: in.Message, ConsentVersion: in.Consent.DocumentVersion, Accepted: in.Consent.Accepted, Website: in.Website})
		if err != nil {
			var invalid *application.ValidationError
			switch {
			case errors.As(err, &invalid):
				status := 422
				if invalid.Code == "InvalidRequest" {
					status = 400
				}
				respond(status, invalid.Code, invalid.Fields)
			case errors.Is(err, application.ErrIdentityConflict):
				respond(409, "SubmissionIdentityConflict", nil)
			default:
				logger.Warn("feedback storage unavailable", "requestID", requestID)
				respond(503, "FeedbackStorageUnavailable", nil)
			}
			return
		}
		status := 200
		if created {
			status = 201
		}
		writeJSON(w, status, receipt, logger)
	}
}

func writeJSON(w http.ResponseWriter, status int, value interface{}, logger *slog.Logger) {
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		logger.Warn("response write failed")
	}
}

func decodeInput(body []byte) (wireInput, error) {
	var in wireInput
	if !utf8.Valid(body) || !validEscapes(body) {
		return in, errors.New("invalid text encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := uniqueKeys(decoder); err != nil {
		return in, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return in, errors.New("trailing input")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return in, err
	}
	if fields == nil {
		return in, errors.New("object required")
	}
	for key, value := range fields {
		switch key {
		case "submissionID", "name", "email", "phone", "message", "website":
			var text string
			if bytes.Equal(value, []byte("null")) {
				return in, errors.New("null field")
			}
			if err := json.Unmarshal(value, &text); err != nil {
				return in, err
			}
		case "consent":
			var consent map[string]json.RawMessage
			if err := json.Unmarshal(value, &consent); err != nil {
				return in, err
			}
			if consent == nil {
				return in, errors.New("consent object required")
			}
			for field, v := range consent {
				if bytes.Equal(v, []byte("null")) {
					return in, errors.New("null consent field")
				}
				switch field {
				case "accepted":
					var b bool
					if err := json.Unmarshal(v, &b); err != nil {
						return in, err
					}
				case "documentVersion":
					var s string
					if err := json.Unmarshal(v, &s); err != nil {
						return in, err
					}
				default:
					return in, errors.New("unknown consent field")
				}
			}
		default:
			return in, errors.New("unknown field")
		}
	}
	strict := json.NewDecoder(bytes.NewReader(body))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&in); err != nil {
		return in, err
	}
	return in, nil
}

func uniqueKeys(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			t, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := t.(string)
			if !ok || seen[key] {
				return errors.New("duplicate or invalid key")
			}
			seen[key] = true
			if err := uniqueKeys(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueKeys(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}

func validEscapes(body []byte) bool {
	quoted := false
	for i := 0; i < len(body); i++ {
		if body[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || body[i] != '\\' {
			continue
		}
		i++
		if i >= len(body) {
			return false
		}
		if body[i] != 'u' {
			continue
		}
		if i+4 >= len(body) {
			return false
		}
		v, err := strconv.ParseUint(string(body[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if v >= 0xdc00 && v <= 0xdfff {
			return false
		}
		if v >= 0xd800 && v <= 0xdbff {
			if i+6 >= len(body) || body[i+1] != '\\' || body[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(body[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return !quoted
}
