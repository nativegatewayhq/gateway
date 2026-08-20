package openai

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

type EditHandler struct {
	common       *Handler
	spoolSlots   chan struct{}
	maxBodyBytes int64
	tempDir      string
}

func NewEditHandler(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executors map[providercredentials.ProviderID]Executor, maxBodyBytes int64, maxConcurrentSpools int) *EditHandler {
	if maxConcurrentSpools < 1 {
		maxConcurrentSpools = 1
	}
	return &EditHandler{common: NewImagesHandler(logger, authenticator, models, executors, maxBodyBytes), spoolSlots: make(chan struct{}, maxConcurrentSpools), maxBodyBytes: maxBodyBytes}
}

func NewBillableEditHandler(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executors map[providercredentials.ProviderID]Executor, maxBodyBytes int64, maxConcurrentSpools int, chargeBilling Billing) *EditHandler {
	handler := NewEditHandler(logger, authenticator, models, executors, maxBodyBytes, maxConcurrentSpools)
	handler.common.billing = chargeBilling
	return handler
}

func (handler *EditHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracked := &statusWriter{ResponseWriter: writer}
	started := time.Now()
	provider := providercredentials.ProviderID("")
	logModel := "invalid"
	defer func() {
		if recover() != nil && !tracked.wroteHeader {
			writeError(tracked, 500, "server_error", "internal_error", "internal server error")
		}
		handler.common.logger.Info("openai image edit request completed", "request_id", requestid.FromContext(request.Context()), "protocol", "openai", "operation", "image.edit", "provider", string(provider), "model", logModel, "status", tracked.statusCode(), "duration", time.Since(started))
	}()
	if request.Method != http.MethodPost {
		tracked.Header().Set("Allow", http.MethodPost)
		writeError(tracked, 405, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}
	principal, authenticated := handler.common.authenticate(tracked, request)
	if !authenticated {
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && mediaType != "multipart/form-data") {
		writeError(tracked, 400, "invalid_request_error", "invalid_content_type", "unsupported edit content type")
		return
	}
	if request.ContentLength > handler.maxBodyBytes {
		writeError(tracked, 413, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	if mediaType == "application/json" {
		body, readErr := readBounded(request.Body, handler.maxBodyBytes)
		if readErr != nil {
			writeError(tracked, 413, "invalid_request_error", "request_too_large", "request body too large")
			return
		}
		model, modelErr := extractModel(body)
		var selector imageoperation.PricingSelector
		if modelErr == nil && handler.common.billing != nil {
			selector, modelErr = imageoperation.ParseOpenAIJSONPricingSelector(body)
			model = selector.Model
		}
		var route imageoperation.ModelRoute
		routed := false
		if modelErr == nil {
			route, routed = handler.routeModel(tracked, model, imageoperation.JSON, &provider, &logModel)
		}
		if modelErr != nil || !routed {
			if modelErr != nil {
				writeError(tracked, 400, "invalid_request_error", "invalid_model", "request must contain one model")
			}
			return
		}
		var begin *billing.BeginRequest
		if handler.common.billing != nil {
			value := billing.BeginRequest{RequestID: requestid.FromContext(request.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, Protocol: "openai", Operation: string(imageoperation.Edit), Model: selector.Model, ChannelID: route.ChannelID, Quantity: selector.Quantity, Size: selector.Size, Quality: selector.Quality}
			begin = &value
		}
		handler.execute(tracked, request, route, begin, request.Header.Get("Content-Type"), int64(len(body)), bytes.NewReader(body))
		return
	}
	boundary := parameters["boundary"]
	if boundary == "" {
		writeError(tracked, 400, "invalid_request_error", "invalid_multipart", "multipart boundary required")
		return
	}
	select {
	case handler.spoolSlots <- struct{}{}:
		defer func() { <-handler.spoolSlots }()
	default:
		writeError(tracked, 503, "server_error", "spool_capacity_exhausted", "edit capacity unavailable")
		return
	}
	file, err := os.CreateTemp(handler.tempDir, "gateway-image-edit-*")
	if err != nil {
		writeError(tracked, 503, "server_error", "spool_unavailable", "edit capacity unavailable")
		return
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	written, err := io.Copy(file, io.LimitReader(request.Body, handler.maxBodyBytes+1))
	if err != nil || written > handler.maxBodyBytes {
		writeError(tracked, 413, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(tracked, 500, "server_error", "internal_error", "internal server error")
		return
	}
	model, err := multipartModel(file, boundary)
	var selector imageoperation.PricingSelector
	if err == nil && handler.common.billing != nil {
		if _, err = file.Seek(0, io.SeekStart); err == nil {
			selector, err = imageoperation.ParseOpenAIMultipartPricingSelector(file, boundary)
			model = selector.Model
		}
	}
	var route imageoperation.ModelRoute
	routed := false
	if err == nil {
		route, routed = handler.routeModel(tracked, model, imageoperation.Multipart, &provider, &logModel)
	}
	if err != nil || !routed {
		if err != nil {
			writeError(tracked, 400, "invalid_request_error", "invalid_model", "request must contain one model")
		}
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(tracked, 500, "server_error", "internal_error", "internal server error")
		return
	}
	var begin *billing.BeginRequest
	if handler.common.billing != nil {
		value := billing.BeginRequest{RequestID: requestid.FromContext(request.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, Protocol: "openai", Operation: string(imageoperation.Edit), Model: selector.Model, ChannelID: route.ChannelID, Quantity: selector.Quantity, Size: selector.Size, Quality: selector.Quality}
		begin = &value
	}
	handler.execute(tracked, request, route, begin, request.Header.Get("Content-Type"), written, file)
}

func (handler *EditHandler) routeModel(writer http.ResponseWriter, model string, media imageoperation.MediaType, provider *providercredentials.ProviderID, logModel *string) (imageoperation.ModelRoute, bool) {
	route, err := handler.common.models.Resolve(model, imageoperation.Edit, media)
	if err != nil {
		if errors.Is(err, imageoperation.ErrModelNotFound) {
			writeError(writer, 404, "invalid_request_error", "model_not_found", "model not found")
		} else {
			writeError(writer, 400, "invalid_request_error", "unsupported_media_type_for_model", "content type is not supported for model")
		}
		return imageoperation.ModelRoute{}, false
	}
	*provider, *logModel = route.Provider, model
	return route, true
}

func (handler *EditHandler) execute(writer http.ResponseWriter, request *http.Request, route imageoperation.ModelRoute, begin *billing.BeginRequest, contentType string, length int64, body io.Reader) {
	var charge *billing.Charge
	defer func() {
		if recover() != nil {
			settled := true
			if charge != nil {
				settled = handler.common.settle(writer, request.Context(), charge.ID, false)
			}
			if settled {
				writeError(writer, http.StatusInternalServerError, "server_error", "internal_error", "internal server error")
			}
		}
	}()
	executor := handler.common.executors[route.Provider]
	if executor == nil {
		writeError(writer, 503, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	if begin != nil {
		started, err := handler.common.billing.Begin(request.Context(), *begin)
		if err != nil {
			handler.common.writeBillingError(writer, err)
			return
		}
		charge = &started
	}
	response, err := executor.Generate(request.Context(), openaiimages.Request{Operation: openaiimages.Edit, ContentType: contentType, ContentLength: length, Accept: request.Header.Get("Accept"), UserAgent: request.UserAgent(), Body: body})
	if err != nil {
		if charge != nil && !handler.common.settle(writer, request.Context(), charge.ID, false) {
			return
		}
		handler.common.writeExecutorError(writer, err)
		return
	}
	defer response.Body.Close()
	if charge != nil && !handler.common.settle(writer, request.Context(), charge.ID, response.StatusCode >= 200 && response.StatusCode <= 299) {
		return
	}
	copyResponseHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

func multipartModel(reader io.Reader, boundary string) (string, error) {
	multipartReader := multipart.NewReader(reader, boundary)
	model := ""
	count := 0
	for {
		part, err := multipartReader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if part.FormName() == "model" {
			count++
			value, err := io.ReadAll(io.LimitReader(part, 202))
			if err != nil || len(value) > 200 {
				return "", errors.New("invalid model")
			}
			model = string(value)
		} else {
			_, _ = io.Copy(io.Discard, part)
		}
		part.Close()
	}
	if count != 1 || model == "" || strings.TrimSpace(model) != model {
		return "", errors.New("invalid model")
	}
	return model, nil
}
