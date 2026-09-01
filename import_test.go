package gttp_test

import (
	"testing"

	"github.com/bluesky-social/gttp"
)

func TestPublicPackageImport(t *testing.T) {
	client := gttp.New(gttp.WithNoRetries())
	if client == nil {
		t.Fatal("gttp.New returned a nil client")
	}
}
