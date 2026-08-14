package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

var errInvalidJSONBody = errors.New("invalid JSON body")

// decodeJSONRequest bounds public request bodies and rejects trailing JSON.
// WebSocket frame limits do not protect these pre-upgrade HTTP endpoints.
func decodeJSONRequest(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64, allowEmpty bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		return errInvalidJSONBody
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errInvalidJSONBody
	}
	return nil
}
