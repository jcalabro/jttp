package jttp

import (
	"crypto/tls"
	"net/http"
	"testing"
)

func TestDefaultTLSHasSessionCache(t *testing.T) {
	client := New(WithNoRetries())
	rt := client.Transport.(*retryTransport)
	tr := rt.next.(*http.Transport)
	requireTrue(t, tr.TLSClientConfig != nil)
	requireTrue(t, tr.TLSClientConfig.ClientSessionCache != nil)
}

func TestCustomTLSSessionCachePreserved(t *testing.T) {
	custom := tls.NewLRUClientSessionCache(8)
	cfg := &tls.Config{ClientSessionCache: custom}
	client := New(WithTLSConfig(cfg), WithNoRetries())
	rt := client.Transport.(*retryTransport)
	tr := rt.next.(*http.Transport)
	requireTrue(t, tr.TLSClientConfig.ClientSessionCache == custom)
}

func TestCustomTLSWithoutSessionCacheGetsDefault(t *testing.T) {
	cfg := &tls.Config{ServerName: "example.com"}
	client := New(WithTLSConfig(cfg), WithNoRetries())
	rt := client.Transport.(*retryTransport)
	tr := rt.next.(*http.Transport)
	requireTrue(t, tr.TLSClientConfig.ClientSessionCache != nil)
}
