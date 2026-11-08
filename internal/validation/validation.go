// Package validation holds the error shape shared by every validator of model output
// (segment, content). One shape means the orchestrator can store any failure in
// llm_calls.validation_errors and turn it into a repair prompt the same way, and the API
// can return it as a 422 body.
package validation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// CodeInvalidJSON is shared: every validator starts by decoding JSON.
const CodeInvalidJSON = "invalid_json"

// FieldError is one problem, located by a JSON path like "rules[1].value" or "sms".
// Code is stable (tests, UI); Message is written for both the marketer and the model.
type FieldError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error lists every problem found, not just the first, so a single repair round can fix
// all of them.
type Error struct {
	Errors []FieldError `json:"errors"`
}

func (e *Error) Error() string {
	msgs := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		msgs[i] = fe.Path + ": " + fe.Message
	}
	return "validation failed: " + strings.Join(msgs, "; ")
}

// OrNil returns nil for an empty list, so callers can `return x, validation.OrNil(errs)`.
func OrNil(errs []FieldError) error {
	if len(errs) == 0 {
		return nil
	}
	return &Error{Errors: errs}
}

// InvalidJSON wraps a decode error as a validation error at the document root.
func InvalidJSON(err error) *Error {
	return &Error{Errors: []FieldError{{Path: "$", Code: CodeInvalidJSON, Message: err.Error()}}}
}

// DecodeStrict decodes one JSON value, rejecting unknown keys and trailing data
// (e.g. a model adding "Sure!" after the object).
func DecodeStrict(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}
