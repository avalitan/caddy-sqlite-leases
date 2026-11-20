package sqliteleases

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Handler wraps an existing registration route and renews only on HTTP 200.
// Place it before the registration handlers, NOT after a terminal static_response.
type Handler struct {
	Kind string `json:"kind"`
	app  *App
}

func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.sqlite_lease", New: func() caddy.Module { return new(Handler) }}
}
func (h *Handler) Provision(ctx caddy.Context) error {
	if h.Kind != "l7" && h.Kind != "l4" {
		return fmt.Errorf("kind must be l7 or l4")
	}
	app, err := ctx.App("sqlite_leases")
	if err != nil {
		return err
	}
	h.app = app.(*App)
	return nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 {
		return caddyhttp.Error(http.StatusForbidden, fmt.Errorf("verified client certificate required"))
	}
	cert := r.TLS.PeerCertificates[0]
	cn := cert.Subject.CommonName
	uris := make([]string, 0, len(cert.URIs))
	for _, u := range cert.URIs {
		uris = append(uris, u.String())
	}
	if _, err := routeID(cn, h.Kind); err != nil {
		return caddyhttp.Error(http.StatusForbidden, err)
	}
	if _, _, err := h.app.resolvePolicy(cn, uris); err != nil {
		return caddyhttp.Error(http.StatusForbidden, err)
	}

	// Hold the same lock across registration and renewal as the cleanup worker.
	h.app.mu.Lock()
	defer h.app.mu.Unlock()
	// Registration endpoints return small JSON responses. Buffer them so a
	// failed lease write cannot accidentally return HTTP 200 to the client.
	const maxResponse = 1 << 20
	rw := &statusWriter{header: make(http.Header), limit: maxResponse}
	err := next.ServeHTTP(rw, r)
	if err != nil {
		return err
	}
	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	if rw.overflow {
		return caddyhttp.Error(http.StatusInternalServerError, fmt.Errorf("registration response exceeded %d bytes", maxResponse))
	}
	if status == http.StatusOK {
		if err := h.app.renew(r.Context(), cn, h.Kind, uris); err != nil {
			return caddyhttp.Error(http.StatusServiceUnavailable, fmt.Errorf("registration succeeded but lease renewal failed: %w", err))
		}
	}
	for key, values := range rw.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(status)
	_, err = io.Copy(w, &rw.body)
	return err
}

type statusWriter struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	limit    int
	overflow bool
}

func (w *statusWriter) Header() http.Header { return w.header }
func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if len(p) > w.limit-w.body.Len() {
		w.overflow = true
		return 0, fmt.Errorf("buffered registration response too large")
	}
	return w.body.Write(p)
}

var _ caddy.Provisioner = (*Handler)(nil)
var _ caddyhttp.MiddlewareHandler = (*Handler)(nil)

func init() {
	caddy.RegisterModule(App{})
	caddy.RegisterModule(Handler{})
}
