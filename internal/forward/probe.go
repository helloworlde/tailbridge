package forward

import (
	"context"
	"net/http"
	"time"

	"github.com/helloworlde/tailbridge/internal/config"
	"github.com/helloworlde/tailbridge/internal/diagnostics"
)

const ProbeTimeout = 5 * time.Second

// ProbeResult deliberately excludes upstream addresses, response bodies,
// headers, credentials and raw errors. Reachable describes transport reachability,
// not application authentication or the health of an arbitrary TCP protocol.
type ProbeResult struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Reachable  bool   `json:"reachable"`
	DurationMS int64  `json:"duration_ms"`
	HTTPStatus int    `json:"http_status,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
}

// Probe checks a configured upstream through the same Tailnet dialer as its
// route. HTTP uses HEAD, validates TLS and never follows redirects.
func Probe(parent context.Context, cfg config.Route, dialer Dialer) (result ProbeResult) {
	result.ID, result.Kind = cfg.ID, cfg.Kind
	started := time.Now()
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()
	ctx, cancel := context.WithTimeout(parent, ProbeTimeout)
	defer cancel()
	if cfg.Kind == "tcp" {
		if cfg.ConnectTimeout.Duration > 0 {
			var cancelDial context.CancelFunc
			ctx, cancelDial = context.WithTimeout(ctx, cfg.ConnectTimeout.Duration)
			defer cancelDial()
		}
		conn, err := dialer.Dial(ctx, "tcp", cfg.Upstream)
		if err != nil {
			result.ErrorClass = diagnostics.Classify(err)
			return
		}
		conn.Close()
		result.Reachable = true
		return
	}
	if cfg.Kind != "http" {
		result.ErrorClass = "invalid_configuration"
		return
	}
	transport, err := newHTTPTransport(cfg, dialer)
	if err != nil {
		result.ErrorClass = "invalid_configuration"
		return
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, cfg.Upstream, nil)
	if err != nil {
		result.ErrorClass = "invalid_configuration"
		return
	}
	if cfg.HostHeader == "preserve" {
		req.Host = cfg.Listen
	}
	req.Header.Set("X-Forwarded-Proto", "http")
	resp, err := client.Do(req)
	if err != nil {
		result.ErrorClass = diagnostics.Classify(err)
		return
	}
	resp.Body.Close()
	result.Reachable, result.HTTPStatus = true, resp.StatusCode
	return
}
