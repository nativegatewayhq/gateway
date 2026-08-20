package config

import (
	"testing"
	"time"
)

func TestOpenAIChatConfiguration(t *testing.T) {
	values := map[string]string{"GATEWAY_DATABASE_URL": "postgres://test", "GATEWAY_OPENAI_CHAT_MODELS": "gpt-4.1,gpt-4o", "GATEWAY_OPENAI_CHAT_REQUEST_TIMEOUT": "45s", "GATEWAY_OPENAI_CHAT_STREAM_IDLE_TIMEOUT": "12s", "GATEWAY_OPENAI_CHAT_MAX_BODY_BYTES": "4096"}
	lookup := func(key string) (string, bool) { v, ok := values[key]; return v, ok }
	cfg, err := Load(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OpenAIChatModels) != 2 || cfg.ChatTimeout != 45*time.Second || cfg.ChatStreamIdleTimeout != 12*time.Second || cfg.ChatBodyBytes != 4096 {
		t.Fatalf("config=%+v", cfg)
	}
	values["GATEWAY_OPENAI_CHAT_MODELS"] = "gpt-4.1,gpt-4.1"
	if _, err := Load(lookup); err == nil {
		t.Fatal("duplicate model accepted")
	}
	values["GATEWAY_OPENAI_CHAT_MODELS"] = "bad model"
	if _, err := Load(lookup); err == nil {
		t.Fatal("invalid model accepted")
	}
	values["GATEWAY_OPENAI_CHAT_MODELS"] = "gpt-4.1"
	values["GATEWAY_BILLING_MODE"] = "required"
	if _, err := Load(lookup); err == nil {
		t.Fatal("unsettled paid Chat accepted")
	}
	values["GATEWAY_OPENAI_CHAT_MODEL_LIMITS"] = "gpt-4.1:128000:16384"
	if cfg, err := Load(lookup); err != nil || cfg.OpenAIChatModelLimits["gpt-4.1"].MaximumOutputTokens != 16384 {
		t.Fatalf("paid config=%+v err=%v", cfg, err)
	}
}

func TestResponsesConfigurationRequiresLimitsWithBilling(t *testing.T) {
	values := map[string]string{"GATEWAY_DATABASE_URL": "postgres://test", "GATEWAY_OPENAI_RESPONSES_MODELS": "gpt-4.1", "GATEWAY_OPENAI_RESPONSES_REQUEST_TIMEOUT": "40s", "GATEWAY_OPENAI_RESPONSES_STREAM_IDLE_TIMEOUT": "7s", "GATEWAY_OPENAI_RESPONSES_MAX_BODY_BYTES": "4096"}
	lookup := func(key string) (string, bool) { v, ok := values[key]; return v, ok }
	cfg, err := Load(lookup)
	if err != nil || len(cfg.OpenAIResponsesModels) != 1 || cfg.ResponsesTimeout != 40*time.Second || cfg.ResponsesStreamIdleTimeout != 7*time.Second || cfg.ResponsesBodyBytes != 4096 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	values["GATEWAY_BILLING_MODE"] = "required"
	if _, err = Load(lookup); err == nil {
		t.Fatal("managed Responses accepted without limits")
	}
	values["GATEWAY_OPENAI_RESPONSES_MODEL_LIMITS"] = "gpt-4.1:128000:16384"
	if cfg, err = Load(lookup); err != nil || cfg.OpenAIResponsesModelLimits["gpt-4.1"].MaximumOutputTokens != 16384 {
		t.Fatalf("paid cfg=%+v err=%v", cfg, err)
	}
}

func TestGeminiLLMConfiguration(t *testing.T) {
	values := map[string]string{"GATEWAY_DATABASE_URL": "postgres://test", "GATEWAY_GEMINI_LLM_MODELS": "gemini-2.5-pro,gemini-2.5-flash", "GATEWAY_GEMINI_STREAM_IDLE_TIMEOUT": "9s"}
	lookup := func(key string) (string, bool) { v, ok := values[key]; return v, ok }
	cfg, err := Load(lookup)
	if err != nil || len(cfg.GeminiLLMModels) != 2 || cfg.GeminiLLMModels[0] != "gemini-2.5-pro" || cfg.GeminiStreamIdleTimeout != 9*time.Second {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	values["GATEWAY_GEMINI_STREAM_IDLE_TIMEOUT"] = "0s"
	if _, err = Load(lookup); err == nil {
		t.Fatal("invalid Gemini stream idle timeout accepted")
	}
	values["GATEWAY_GEMINI_STREAM_IDLE_TIMEOUT"] = "9s"
	for _, invalid := range []string{"gemini-image", "gemini-2.5-pro,gemini-2.5-pro", "bad model", ""} {
		values["GATEWAY_GEMINI_LLM_MODELS"] = invalid
		if _, err = Load(lookup); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
	values["GATEWAY_GEMINI_LLM_MODELS"] = "gemini-2.5-pro"
	values["GATEWAY_BILLING_MODE"] = "required"
	if _, err = Load(lookup); err == nil {
		t.Fatal("managed Gemini LLM accepted without limits")
	}
	values["GATEWAY_GEMINI_LLM_MODEL_LIMITS"] = "gemini-2.5-pro:1048576:65536"
	cfg, err = Load(lookup)
	if err != nil || cfg.GeminiLLMModelLimits["gemini-2.5-pro"].MaximumOutputTokens != 65536 {
		t.Fatalf("managed cfg=%+v err=%v", cfg, err)
	}
}

func TestAnthropicConfigurationRequiresLimitsWithBilling(t *testing.T) {
	values := map[string]string{"GATEWAY_DATABASE_URL": "postgres://test", "GATEWAY_ANTHROPIC_MESSAGES_MODELS": "claude-test", "GATEWAY_ANTHROPIC_STREAM_IDLE_TIMEOUT": "9s", "GATEWAY_BILLING_MODE": "required"}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	if _, err := Load(lookup); err == nil {
		t.Fatal("managed Anthropic accepted without limits")
	}
	values["GATEWAY_ANTHROPIC_MESSAGES_MODEL_LIMITS"] = "claude-test:200000:8192"
	cfg, err := Load(lookup)
	if err != nil || cfg.AnthropicMessagesModelLimits["claude-test"].MaximumOutputTokens != 8192 || cfg.AnthropicStreamIdleTimeout != 9*time.Second {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestOpenAIChatRoutesJSONDerivesModelsAndLimits(t *testing.T) {
	values := map[string]string{
		"GATEWAY_DATABASE_URL":            "postgres://test",
		"GATEWAY_BILLING_MODE":            "required",
		"GATEWAY_OPENAI_CHAT_ROUTES_JSON": `[{"model":"logical-chat","owner":"gateway","policy":"priority","maximum_input_tokens":4096,"maximum_output_tokens":512,"candidates":[{"id":"candidate_openai","provider":"openai","provider_model":"gpt-4.1","channel_id":"channel_00000000000000000000000000000001","enabled":true,"streaming":true,"tools":true,"json_mode":true}]}]`,
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	cfg, err := Load(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OpenAIChatRoutes) != 1 || len(cfg.OpenAIChatModels) != 1 || cfg.OpenAIChatModels[0] != "logical-chat" || cfg.OpenAIChatModelLimits["logical-chat"].MaximumOutputTokens != 512 {
		t.Fatalf("routes=%+v models=%v", cfg.OpenAIChatRoutes, cfg.OpenAIChatModels)
	}
}

func TestOpenAIChatRoutesJSONRejectsLegacyCombination(t *testing.T) {
	values := map[string]string{"GATEWAY_DATABASE_URL": "postgres://test", "GATEWAY_OPENAI_CHAT_MODELS": "gpt-4.1", "GATEWAY_OPENAI_CHAT_ROUTES_JSON": `[{"model":"logical"}]`}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	if _, err := Load(lookup); err == nil {
		t.Fatal("combined route configuration accepted")
	}
}
