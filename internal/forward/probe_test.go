package forward

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helloworlde/tailbridge/internal/config"
)

func TestProbeHTTPReportsResponseWithoutFollowingRedirect(t *testing.T) {
	seen := make(chan [2]string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- [2]string{r.Method, r.URL.Path}
		w.Header().Set("Location", "http://must-not-contact.invalid/private")
		w.WriteHeader(http.StatusFound)
		io.WriteString(w, "private response")
	}))
	defer upstream.Close()
	result := Probe(context.Background(), config.Route{
		ID: "dashboard", Kind: "http", Upstream: upstream.URL + "/dashboard", HostHeader: "upstream",
		ConnectTimeout: config.Duration{Duration: time.Second},
	}, &directDialer{})
	if !result.Reachable || result.HTTPStatus != http.StatusFound || result.ErrorClass != "" {
		t.Fatalf("probe result=%+v", result)
	}
	request := <-seen
	if request[0] != http.MethodHead || request[1] != "/dashboard" {
		t.Fatalf("probe sent %v", request)
	}
	encoded, _ := json.Marshal(result)
	for _, secret := range []string{upstream.URL, "must-not-contact", "private response"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("probe leaked %q", secret)
		}
	}
}

func TestProbeTLSValidatesTrustAndServerName(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	upstream.Config.ErrorLog = log.New(io.Discard, "", 0)
	upstream.StartTLS()
	defer upstream.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, ca, sni string
		reachable     bool
	}{
		{name: "untrusted certificate"},
		{name: "trusted private CA", ca: caFile, reachable: true},
		{name: "wrong server name", ca: caFile, sni: "wrong.invalid"},
		{name: "explicit matching server name", ca: caFile, sni: "example.com", reachable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := Probe(context.Background(), config.Route{ID: "tls", Kind: "http", Upstream: upstream.URL, CAFile: tc.ca, TLSServerName: tc.sni, ConnectTimeout: config.Duration{Duration: time.Second}}, &directDialer{})
			if result.Reachable != tc.reachable || (tc.reachable && result.HTTPStatus != 204) || (!tc.reachable && result.ErrorClass != "tls_failed") {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestProbeTCPConnectsAndHonorsCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	result := Probe(context.Background(), config.Route{ID: "tcp", Kind: "tcp", Upstream: listener.Addr().String()}, &directDialer{})
	if !result.Reachable {
		t.Fatalf("result=%+v", result)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := conn.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("probe should send no application data: n=%d err=%v", n, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result = Probe(ctx, config.Route{ID: "timeout", Kind: "tcp", Upstream: "private.invalid:22"}, dialFunc(func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }))
	if result.Reachable || result.ErrorClass != "target_timeout" || result.DurationMS > 500 {
		t.Fatalf("timeout result=%+v", result)
	}
}
