package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

func respondJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		// The response may already be committed. The request logger captures the
		// resulting status, while handlers avoid exposing encoder internals.
		return
	}
}

func respondError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	respondJSON(w, status, ErrorResponse{Error: ErrorBody{Code: code, Message: message, RequestID: requestIDFromContext(r.Context())}})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		return errors.New("Content-Type must be application/json")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		var maximumBytesError *http.MaxBytesError
		if errors.As(err, &maximumBytesError) {
			return maximumBytesError
		}
		if errors.Is(err, io.EOF) {
			return errors.New("request body must not be empty")
		}
		return errors.New("request body is not valid JSON or contains unknown fields")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain exactly one JSON value")
	}
	return nil
}

func writeDecodeError(w http.ResponseWriter, r *http.Request, err error) {
	var maximumBytesError *http.MaxBytesError
	if errors.As(err, &maximumBytesError) || strings.Contains(err.Error(), "request body too large") {
		respondError(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit")
		return
	}
	respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
}
