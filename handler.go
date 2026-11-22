package sqliteleases

import (
	"fmt"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Handler renews an authenticated registration lease before the success response.
// Place it ONLY inside a confirmed successful registration response branch,
// immediately before the terminal static_response handler.
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

	// This handler runs only after the caller has verified registration success.
	// Serialize the lease write against cleanup, but never hold the lock while
	// running subsequent handlers (which may perform Admin API operations).
	h.app.mu.Lock()
	err := h.app.renew(r.Context(), cn, h.Kind, uris)
	h.app.mu.Unlock()
	if err != nil {
		return caddyhttp.Error(http.StatusServiceUnavailable, fmt.Errorf("lease renewal failed: %w", err))
	}

	return next.ServeHTTP(w, r)
}

var _ caddy.Provisioner = (*Handler)(nil)
var _ caddyhttp.MiddlewareHandler = (*Handler)(nil)

func init() {
	caddy.RegisterModule(App{})
	caddy.RegisterModule(Handler{})
}
