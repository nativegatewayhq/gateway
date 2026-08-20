package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestCommandsRejectInvalidInputBeforeDatabaseAccess(t *testing.T) {
	service := NewService(nil)
	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{"deposit amount", func() error { _, err := service.Deposit(context.Background(), "org_valid", 0, "key"); return err }, ErrInvalidAmount},
		{"deposit organization", func() error { _, err := service.Deposit(context.Background(), "invalid", 1, "key"); return err }, ErrInvalidIdentifier},
		{"reserve amount", func() error {
			_, err := service.Reserve(context.Background(), "org_valid", "project_valid", "request", -1, "key")
			return err
		}, ErrInvalidAmount},
		{"reserve project", func() error {
			_, err := service.Reserve(context.Background(), "org_valid", "invalid", "request", 1, "key")
			return err
		}, ErrInvalidIdentifier},
		{"capture amount", func() error { _, err := service.Capture(context.Background(), "res_valid", -1, "key"); return err }, ErrInvalidAmount},
		{"release reservation", func() error { _, err := service.Release(context.Background(), "invalid", "key"); return err }, ErrInvalidIdentifier},
		{"refund amount", func() error { _, err := service.Refund(context.Background(), "res_valid", 0, "key"); return err }, ErrInvalidAmount},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, test.want) {
				t.Fatalf("error=%v want %v", err, test.want)
			}
		})
	}
}

func TestIDEntropyFailureIsReturned(t *testing.T) {
	service := newService(nil, bytes.NewReader(nil))
	if _, err := service.id("led_"); err == nil {
		t.Fatal("id generation succeeded without entropy")
	}
}
