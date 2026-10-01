package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

var (
	errEmptyBody    = &httpError{http.StatusBadRequest, codeInvalid, "the request body is empty", false}
	errNotJSONBody  = &httpError{http.StatusBadRequest, codeInvalid, "the request body is not valid JSON", false}
	errNotAnObject  = &httpError{http.StatusBadRequest, codeInvalid, "the request body must be a JSON object", false}
	errTrailingData = &httpError{http.StatusBadRequest, codeInvalid, "unexpected data after the JSON object", false}
	errBodyNotValid = &httpError{http.StatusBadRequest, codeInvalid, "the request body is not valid", false}
)

// decode reads the JSON object of a request into dst. An empty body is an
// error unless optional, which leaves dst as it is.
func decode(c *gin.Context, dst any, optional bool) error {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	switch {
	case errors.Is(err, io.EOF):
		if optional {
			return nil
		}
		return errEmptyBody
	case err != nil:
		return bodyError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errTrailingData
	}
	return nil
}

// bodyError describes a body that could not be decoded. It says what is wrong
// without repeating what was sent: a syntax error of encoding/json quotes the
// offending character, and a secret may be what it quotes.
func bodyError(err error) *httpError {
	var (
		syntax *json.SyntaxError
		typ    *json.UnmarshalTypeError
	)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &syntax):
		return errNotJSONBody
	case errors.As(err, &typ) && typ.Field != "":
		return &httpError{http.StatusBadRequest, codeInvalid, fmt.Sprintf("the field %q has the wrong type", typ.Field), false}
	case errors.As(err, &typ):
		return errNotAnObject
	}
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return &httpError{http.StatusBadRequest, codeInvalid, "unknown field " + field, false}
	}
	return errBodyNotValid
}
