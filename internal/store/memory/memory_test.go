package memory

import (
	"testing"

	"atlassian-mcp/internal/store"
	"atlassian-mcp/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return New() })
}
