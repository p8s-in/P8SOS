package netx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSystemLookupFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	// Go's resolver can't find "fedora.invalid"; the system resolver can
	Getent = func(ctx context.Context, host string) ([]byte, error) {
		if host != "fedora.invalid" {
			return nil, errors.New("exit status 2")
		}
		return []byte("127.0.0.1       STREAM fedora.invalid\n127.0.0.1       DGRAM\n::1  STREAM\n"), nil
	}
	resp, err := Client(0).Get("http://fedora.invalid:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ips, err := SystemLookup(context.Background(), "fedora.invalid")
	if err != nil || len(ips) != 2 || ips[0].String() != "127.0.0.1" {
		t.Fatalf("%v %v", ips, err)
	}
	publicDNS = nil
	if _, err := Client(0).Get("http://nothing.invalid:" + port + "/"); err == nil {
		t.Fatal("unknown host worked")
	}
}
