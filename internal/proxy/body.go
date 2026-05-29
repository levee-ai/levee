package proxy

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const maxBodySize = 10 * 1024 * 1024 // 10MB

// RequestInfo holds extracted fields from the request body.
type RequestInfo struct {
	Model  string
	Stream bool
}

var errInvalidBody = errors.New("invalid request body")

// readRequestBody reads the request body and extracts model and stream fields.
// If the content type is not JSON, it returns nil, nil, nil.
// If the body is empty or the model field is missing/invalid, it returns errInvalidBody.
func readRequestBody(req *http.Request) (*RequestInfo, []byte, error) {
	if !isJSONContentType(req.Header.Get("Content-Type")) {
		return nil, nil, nil
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, maxBodySize))
	if err != nil {
		return nil, nil, err
	}

	if len(body) == 0 {
		return nil, nil, errInvalidBody
	}

	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.Str == "" {
		return nil, nil, errInvalidBody
	}

	stream := gjson.GetBytes(body, "stream").Bool()

	return &RequestInfo{
		Model:  modelResult.Str,
		Stream: stream,
	}, body, nil
}

// isJSONContentType checks whether the content type indicates JSON,
// ignoring parameters like charset.
func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "application/json")
}
