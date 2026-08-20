package api

import (
	_ "embed"
	"testing"
)

//go:embed handlers.go
var handlersGo string

func handlersSource(t *testing.T) string {
	t.Helper()
	return handlersGo
}
