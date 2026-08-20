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
	values := map[string]string{"GATEWAY_DATABASE_URL": "postgres://test", "GATEWAY_OPENAI_RESPONSES_MODELS": "gpt-4.1", "GATEWAY_OPENAI_RESPONSES_REQUEST_TIMEOUT": "40s", "GATEWAY_OPENAI_RESPONSES_MAX_BODY_BYTES": "4096"}
	lookup := func(key string) (string, bool) { v, ok := values[key]; return v, ok }
	cfg, err := Load(lookup)
	if err != nil || len(cfg.OpenAIResponsesModels) != 1 || cfg.ResponsesTimeout != 40*time.Second || cfg.ResponsesBodyBytes != 4096 {
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
