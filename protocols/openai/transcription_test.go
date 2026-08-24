package openai

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/audiobilling"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	audiooperation "github.com/nativegatewayhq/gateway/operations/audio"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type transcriptionExecutorFunc func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error)

type transcriptionBillingStub struct {
	begin             audiobilling.TranscriptionBeginRequest
	beginCalls        int
	pendingAfterFirst bool
	complete          *audiobilling.TranscriptionEvidence
	completed         []audiobilling.TranscriptionEvidence
	released          string
	reconciling       string
}

type failSecondTranscriptionWrite struct {
	header http.Header
	writes int
}

func (w *failSecondTranscriptionWrite) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (*failSecondTranscriptionWrite) WriteHeader(int) {}
func (w *failSecondTranscriptionWrite) Write(body []byte) (int, error) {
	w.writes++
	if w.writes == 2 {
		return 0, context.Canceled
	}
	return len(body), nil
}
func (*failSecondTranscriptionWrite) Flush() {}

func (s *transcriptionBillingStub) Begin(_ context.Context, request audiobilling.TranscriptionBeginRequest) (audiobilling.TranscriptionCharge, error) {
	s.beginCalls++
	if s.pendingAfterFirst && s.beginCalls > 1 {
		return audiobilling.TranscriptionCharge{}, audiobilling.ErrPending
	}
	s.begin = request
	return audiobilling.TranscriptionCharge{ID: "atc_00000000000000000000000000000001"}, nil
}

func TestBillableTranscriptionDuplicateAndExecutorFaultsNeverRedispatch(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		billing := &transcriptionBillingStub{pendingAfterFirst: true}
		calls := 0
		handler := NewBillableTranscriptionHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"text":"private","usage":{"type":"tokens","input_tokens":1,"input_token_details":{"audio_tokens":1,"text_tokens":0},"output_tokens":1,"total_tokens":2}}`))}, nil
		}), providerhealth.NoopGate{}, 4096, 2048, 64, 4096, 1, billing)
		for i := 0; i < 2; i++ {
			contentType, body := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"file", "a.wav", "audio"}})
			request := transcriptionRequest(contentType, body)
			request.Header.Set("Idempotency-Key", "duplicate-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if i == 1 && response.Code != http.StatusConflict {
				t.Fatalf("duplicate status=%d body=%s", response.Code, response.Body.String())
			}
		}
		if calls != 1 || billing.beginCalls != 2 {
			t.Fatalf("provider calls=%d begin calls=%d", calls, billing.beginCalls)
		}
	})
	for _, tc := range []struct {
		name   string
		err    error
		panics bool
	}{{"timeout", openaiProvider.ErrTranscriptionTimeout, false}, {"reset", openaiProvider.ErrTranscriptionUpstream, false}, {"cancel", openaiProvider.ErrTranscriptionCanceled, false}, {"panic", nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			billing := &transcriptionBillingStub{}
			calls := 0
			handler := NewBillableTranscriptionHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
				calls++
				if tc.panics {
					panic("provider panic")
				}
				return nil, tc.err
			}), providerhealth.NoopGate{}, 4096, 2048, 64, 4096, 1, billing)
			contentType, body := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"file", "a.wav", "audio"}})
			request := transcriptionRequest(contentType, body)
			request.Header.Set("Idempotency-Key", "fault-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if calls != 1 || billing.reconciling != "executor_uncertain" || response.Code < 499 {
				t.Fatalf("status=%d calls=%d reconcile=%s body=%s", response.Code, calls, billing.reconciling, response.Body.String())
			}
		})
	}
}
func (s *transcriptionBillingStub) Complete(_ context.Context, _ string, evidence audiobilling.TranscriptionEvidence) (audiobilling.TranscriptionCharge, error) {
	s.complete = &evidence
	s.completed = append(s.completed, evidence)
	return audiobilling.TranscriptionCharge{ID: "atc_00000000000000000000000000000001", State: "CAPTURED"}, nil
}
func (s *transcriptionBillingStub) Release(_ context.Context, _ string, reason string) (audiobilling.TranscriptionCharge, error) {
	s.released = reason
	return audiobilling.TranscriptionCharge{State: "RELEASED"}, nil
}
func (s *transcriptionBillingStub) MarkReconciling(_ context.Context, _ string, reason string, _ *audiobilling.TranscriptionEvidence) error {
	s.reconciling = reason
	return nil
}

func (f transcriptionExecutorFunc) Create(ctx context.Context, r openaiProvider.TranscriptionRequest) (*http.Response, error) {
	return f(ctx, r)
}
func transcriptionBody(t *testing.T, parts []struct{ name, filename, value string }) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, p := range parts {
		if p.filename != "" {
			part, err := w.CreateFormFile(p.name, p.filename)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = part.Write([]byte(p.value))
		} else {
			if err := w.WriteField(p.name, p.value); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), body.Bytes()
}
func transcriptionRegistry(t *testing.T, stream bool) *audiooperation.TranscriptionRegistry {
	t.Helper()
	r, err := audiooperation.NewTranscriptionRegistry([]string{"gpt-4o-transcribe"}, map[string]audiooperation.TranscriptionCapabilities{"gpt-4o-transcribe": {Streaming: stream, ResponseFormats: []string{"json", "text"}, Language: true, Prompt: true}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func transcriptionRequest(contentType string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer service-secret")
	r.Header.Set("Content-Type", contentType)
	return r
}

func TestTranscriptionRebuildsMultipartAndPreservesNativeResponse(t *testing.T) {
	contentType, body := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"language", "", "ko"}, {"future_option", "", "native"}, {"file", "음성.wav", "audio-bytes"}})
	calls := 0
	handler := NewTranscriptionHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(_ context.Context, r openaiProvider.TranscriptionRequest) (*http.Response, error) {
		calls++
		media, params, err := mime.ParseMediaType(r.ContentType)
		if err != nil || media != "multipart/form-data" || r.ContentLength < 1 {
			t.Fatalf("request=%+v", r)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		got := map[string]string{}
		filename := ""
		for {
			part, e := reader.NextPart()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
			value, _ := io.ReadAll(part)
			got[part.FormName()] = string(value)
			if part.FormName() == "file" {
				filename = part.FileName()
			}
		}
		if got["model"] != "gpt-4o-transcribe" || got["language"] != "ko" || got["future_option"] != "native" || got["file"] != "audio-bytes" || filename != "음성.wav" {
			t.Fatalf("got=%v filename=%s", got, filename)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"secret"}}, Body: io.NopCloser(strings.NewReader(`{"text":"안녕"}`))}, nil
	}), providerhealth.NoopGate{}, 1<<20, 1<<19, 1024, 4096, 2)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, transcriptionRequest(contentType, body))
	if response.Code != 200 || response.Body.String() != `{"text":"안녕"}` || response.Header().Get("X-Request-Id") != "" || calls != 1 {
		t.Fatalf("status=%d headers=%v body=%s calls=%d", response.Code, response.Header(), response.Body.String(), calls)
	}
}

func TestTranscriptionRejectsDuplicateTraversalOversizeAndCapabilityBeforeProvider(t *testing.T) {
	calls := 0
	handler := NewTranscriptionHandler(slog.Default(), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
		calls++
		return nil, nil
	}), providerhealth.NoopGate{}, 4096, 5, 16, 4096, 1)
	cases := [][]struct{ name, filename, value string }{{{"model", "", "gpt-4o-transcribe"}, {"model", "", "other"}, {"file", "a.wav", "ok"}}, {{"model", "", "gpt-4o-transcribe"}, {"file", "../secret.wav", "ok"}}, {{"model", "", "gpt-4o-transcribe"}, {"file", "a.wav", "123456"}}, {{"model", "", "gpt-4o-transcribe"}, {"stream", "", "true"}, {"file", "a.wav", "ok"}}, {{"model", "", "gpt-4o-transcribe"}, {"authorization", "", "secret"}, {"file", "a.wav", "ok"}}}
	for _, parts := range cases {
		ct, body := transcriptionBody(t, parts)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, transcriptionRequest(ct, body))
		if response.Code < 400 || response.Code >= 500 {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("provider calls=%d", calls)
	}
}

func TestTranscriptionSpoolSpillsSecurelyAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	spool := newTranscriptionSpool(dir, 4, 16)
	if _, err := spool.ReadFrom(strings.NewReader("123456")); err != nil {
		t.Fatal(err)
	}
	if spool.path == "" {
		t.Fatal("did not spill")
	}
	info, err := os.Stat(spool.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
	source, err := spool.Open()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(source)
	source.Close()
	if string(body) != "123456" {
		t.Fatal(string(body))
	}
	path := spool.path
	spool.Cleanup()
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("spool remains: %v", err)
	}
}

func TestTranscriptionFilenameValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{{"음성 파일.wav", true}, {"../secret.wav", false}, {"folder/audio.wav", false}, {"/tmp/audio.wav", false}, {"audio\x00.wav", false}, {"audio\r.wav", false}, {"audio\n.wav", false}, {strings.Repeat("a", 256), false}} {
		if got := validTranscriptionFilename(tc.name); got != tc.valid {
			t.Fatalf("filename=%q valid=%t want=%t", tc.name, got, tc.valid)
		}
	}
}

func TestTranscriptionCleansSpilledInputWhenLaterPartIsInvalid(t *testing.T) {
	dir := t.TempDir()
	handler := NewTranscriptionHandler(slog.Default(), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
		t.Fatal("provider must not be called")
		return nil, nil
	}), providerhealth.NoopGate{}, 3<<20, 2<<20, 64, 4096, 1)
	handler.tempDir = dir
	contentType, body := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"file", "a.wav", strings.Repeat("a", int(transcriptionMemoryThreshold+1))}, {"model", "", "duplicate"}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, transcriptionRequest(contentType, body))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files remain: entries=%v err=%v", entries, err)
	}
}

func TestTranscriptionStreamsSSEOnlyForCapableModel(t *testing.T) {
	ct, body := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"stream", "", "true"}, {"file", "a.wav", "ok"}})
	handler := NewTranscriptionHandler(slog.Default(), acceptingAuth(t), transcriptionRegistry(t, true), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"delta\":\"ok\"}\n\n"))}, nil
	}), providerhealth.NoopGate{}, 4096, 1024, 64, 4096, 1)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, transcriptionRequest(ct, body))
	if response.Code != 200 || response.Body.String() != "data: {\"delta\":\"ok\"}\n\n" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestTranscriptionRejectsRequestFieldPartAndHeaderLimitsBeforeProvider(t *testing.T) {
	calls := 0
	handler := NewTranscriptionHandler(slog.Default(), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
		calls++
		return nil, nil
	}), providerhealth.NoopGate{}, 1024, 512, 8, 4096, 1)

	fieldType, fieldBody := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"prompt", "", "123456789"}, {"file", "a.wav", "ok"}})
	requestType, requestBody := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"file", "a.wav", strings.Repeat("a", 900)}})
	var headersBody bytes.Buffer
	headersWriter := multipart.NewWriter(&headersBody)
	for _, field := range []struct{ name, value string }{{"model", "gpt-4o-transcribe"}, {"file", "ok"}} {
		headers := textproto.MIMEHeader{"Content-Disposition": {`form-data; name="` + field.name + `"`}}
		if field.name == "file" {
			headers.Set("Content-Disposition", `form-data; name="file"; filename="a.wav"`)
		}
		for i := 0; i < maximumTranscriptionPartHeaderNames; i++ {
			headers.Set("X-Limit-"+string(rune('a'+i)), "value")
		}
		part, err := headersWriter.CreatePart(headers)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte(field.value))
	}
	_ = headersWriter.Close()

	parts := make([]struct{ name, filename, value string }, 0, maximumTranscriptionParts+1)
	parts = append(parts, struct{ name, filename, value string }{"model", "", "gpt-4o-transcribe"})
	for i := 0; i < maximumTranscriptionParts-1; i++ {
		parts = append(parts, struct{ name, filename, value string }{name: "field_" + string(rune('a'+i)), value: "x"})
	}
	parts = append(parts, struct{ name, filename, value string }{"file", "a.wav", "ok"})
	partsType, partsBody := transcriptionBody(t, parts)

	for _, tc := range []struct {
		name        string
		contentType string
		body        []byte
	}{{"field", fieldType, fieldBody}, {"request", requestType, requestBody}, {"headers", headersWriter.FormDataContentType(), headersBody.Bytes()}, {"parts", partsType, partsBody}} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, transcriptionRequest(tc.contentType, tc.body))
			if response.Code < 400 || response.Code >= 500 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if calls != 0 {
		t.Fatalf("provider calls=%d", calls)
	}
}

func TestTranscriptionSpoolCapacityFailsBeforeProvider(t *testing.T) {
	calls := 0
	handler := NewTranscriptionHandler(slog.Default(), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
		calls++
		return nil, nil
	}), providerhealth.NoopGate{}, 4096, 2048, 64, 4096, 1)
	handler.spoolSlots <- struct{}{}
	defer func() { <-handler.spoolSlots }()
	contentType, body := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"file", "a.wav", "ok"}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, transcriptionRequest(contentType, body))
	if response.Code != http.StatusServiceUnavailable || calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
	}
}

func TestTranscriptionDoesNotRedispatchAndCleansTemporaryFilesOnExecutorFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		panic bool
	}{{"timeout", openaiProvider.ErrTranscriptionTimeout, false}, {"reset", openaiProvider.ErrTranscriptionUpstream, false}, {"panic", nil, true}, {"cancel", openaiProvider.ErrTranscriptionCanceled, false}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := 0
			handler := NewTranscriptionHandler(slog.Default(), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
				calls++
				if tc.panic {
					panic("provider panic")
				}
				return nil, tc.err
			}), providerhealth.NoopGate{}, 3<<20, 2<<20, 64, 4096, 1)
			handler.tempDir = dir
			contentType, body := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"file", "a.wav", strings.Repeat("a", int(transcriptionMemoryThreshold+1))}})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, transcriptionRequest(contentType, body))
			if response.Code < 499 || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary files remain: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestTranscriptionClientWriteFailureDoesNotRedispatchAndCleansTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	handler := NewTranscriptionHandler(slog.Default(), acceptingAuth(t), transcriptionRegistry(t, true), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: ok\n\n"))}, nil
	}), providerhealth.NoopGate{}, 3<<20, 2<<20, 64, 4096, 1)
	handler.tempDir = dir
	contentType, body := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"stream", "", "true"}, {"file", "a.wav", strings.Repeat("a", int(transcriptionMemoryThreshold+1))}})
	handler.ServeHTTP(errorWriter{}, transcriptionRequest(contentType, body))
	if calls != 1 {
		t.Fatalf("provider calls=%d", calls)
	}
	entries, err := filepath.Glob(filepath.Join(dir, "gateway-transcription-*"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files remain: entries=%v err=%v", entries, err)
	}
}

func TestBillableTranscriptionCapturesTypedUsageAndPreservesNativeBody(t *testing.T) {
	billing := &transcriptionBillingStub{}
	body := `{"text":"private transcript","usage":{"type":"tokens","input_tokens":4,"input_token_details":{"audio_tokens":3,"text_tokens":1},"output_tokens":2,"total_tokens":6}}`
	handler := NewBillableTranscriptionHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}), providerhealth.NoopGate{}, 4096, 2048, 64, 4096, 1, billing)
	contentType, requestBody := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"file", "a.wav", "audio"}})
	request := transcriptionRequest(contentType, requestBody)
	request.Header.Set("Idempotency-Key", "billing-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != body || billing.begin.IdempotencyKey != "billing-key" || billing.begin.Fingerprint == ([32]byte{}) || billing.complete == nil || billing.complete.Usage.TotalTokens != 6 || billing.released != "" || billing.reconciling != "" {
		t.Fatalf("status=%d body=%s begin=%+v complete=%+v released=%s reconciling=%s", response.Code, response.Body.String(), billing.begin, billing.complete, billing.released, billing.reconciling)
	}
}

func TestBillableTranscriptionReleaseReconcileAndFormatBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, format, providerBody, wantRelease, wantReconcile string
		providerStatus                                         int
		wantCalls                                              int
	}{{"known failure", "json", `{"error":"bad"}`, "provider_non_2xx", "", 400, 1}, {"missing usage", "json", `{"text":"private"}`, "", "usage_invalid", 200, 1}, {"text fail closed", "text", "private", "", "", 200, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			billing := &transcriptionBillingStub{}
			calls := 0
			handler := NewBillableTranscriptionHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
				calls++
				contentType := "application/json"
				if tc.format == "text" {
					contentType = "text/plain"
				}
				return &http.Response{StatusCode: tc.providerStatus, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(tc.providerBody))}, nil
			}), providerhealth.NoopGate{}, 4096, 2048, 64, 4096, 1, billing)
			parts := []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"file", "a.wav", "audio"}}
			if tc.format != "json" {
				parts = append(parts, struct{ name, filename, value string }{"response_format", "", tc.format})
			}
			contentType, requestBody := transcriptionBody(t, parts)
			request := transcriptionRequest(contentType, requestBody)
			request.Header.Set("Idempotency-Key", "billing-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if calls != tc.wantCalls || billing.released != tc.wantRelease || billing.reconciling != tc.wantReconcile {
				t.Fatalf("status=%d calls=%d release=%s reconcile=%s body=%s", response.Code, calls, billing.released, billing.reconciling, response.Body.String())
			}
		})
	}
}

func TestBillableTranscriptionCapturesTerminalSSEUsage(t *testing.T) {
	billing := &transcriptionBillingStub{}
	stream := "data: {\"type\":\"transcript.text.delta\",\"delta\":\"private\"}\n\n" +
		"data: {\"type\":\"transcript.text.done\",\"text\":\"private transcript\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":2,\"input_token_details\":{\"audio_tokens\":2,\"text_tokens\":0},\"output_tokens\":1,\"total_tokens\":3}}\n\n"
	handler := NewBillableTranscriptionHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), transcriptionRegistry(t, true), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}), providerhealth.NoopGate{}, 4096, 2048, 64, 4096, 1, billing)
	contentType, requestBody := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"stream", "", "true"}, {"file", "a.wav", "audio"}})
	request := transcriptionRequest(contentType, requestBody)
	request.Header.Set("Idempotency-Key", "stream-billing-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != stream || billing.complete == nil || billing.complete.SchemaVersion != "openai-transcription-token-sse-v1" || billing.complete.Usage.TotalTokens != 3 || billing.reconciling != "" {
		t.Fatalf("status=%d body=%q complete=%+v reconcile=%s", response.Code, response.Body.String(), billing.complete, billing.reconciling)
	}
}

func TestBillableTranscriptionCapturesObservedTerminalUsageOnClientDisconnect(t *testing.T) {
	billing := &transcriptionBillingStub{}
	stream := "data: {\"type\":\"transcript.text.done\",\"text\":\"private\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":1,\"input_token_details\":{\"audio_tokens\":1,\"text_tokens\":0},\"output_tokens\":1,\"total_tokens\":2}}\n\n"
	handler := NewBillableTranscriptionHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), transcriptionRegistry(t, true), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}), providerhealth.NoopGate{}, 4096, 2048, 64, 4096, 1, billing)
	contentType, requestBody := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"stream", "", "true"}, {"file", "a.wav", "audio"}})
	request := transcriptionRequest(contentType, requestBody)
	request.Header.Set("Idempotency-Key", "disconnect-billing-key")
	writer := &failSecondTranscriptionWrite{}
	handler.ServeHTTP(writer, request)
	if billing.complete == nil || billing.complete.Usage.TotalTokens != 2 || billing.reconciling != "" {
		t.Fatalf("complete=%+v reconcile=%s writes=%d", billing.complete, billing.reconciling, writer.writes)
	}
}
