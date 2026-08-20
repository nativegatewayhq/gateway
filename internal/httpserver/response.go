package httpserver

import (
	"encoding/json"
	"net/http"

	"github.com/nativegatewayhq/gateway/internal/requestid"
)

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, request *http.Request, status int, code, message string) {
	writeJSON(writer, status, errorEnvelope{Error: errorBody{
		Code:      code,
		Message:   message,
		RequestID: requestid.FromContext(request.Context()),
	}})
}
