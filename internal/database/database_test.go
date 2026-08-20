package database

import (
	"context"
	"strings"
	"testing"
)

func TestOpenDoesNotRenderDatabaseURL(t *testing.T) {
	marker := "super-secret-password"
	_, err := Open(context.Background(), "postgres://gateway:"+marker+"@127.0.0.1:not-a-port/gateway")
	if err == nil {
		t.Fatal("Open() error=nil")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error leaked database URL: %v", err)
	}
	if err.Error() != "database connection failed" {
		t.Fatalf("unsafe error=%q", err)
	}
}
