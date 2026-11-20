package sqliteleases

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	_ "modernc.org/sqlite"
)

// App owns the lease database and the single cleanup worker.
type App struct {
	Database        string                    `json:"database,omitempty"`
	LeaseTTL        caddy.Duration            `json:"lease_ttl,omitempty"`
	CleanupInterval *caddy.Duration           `json:"cleanup_interval,omitempty"`
	PolicyURIPrefix string                    `json:"policy_uri_prefix,omitempty"`
	LeasePolicies   map[string]caddy.Duration `json:"lease_policies,omitempty"`
	HostOverrides   map[string]caddy.Duration `json:"host_overrides,omitempty"`
	AdminSocket     string                    `json:"admin_socket,omitempty"`

	mu     sync.Mutex
	db     *sql.DB
	client *http.Client
	cancel context.CancelFunc
	done   chan struct{}
}

func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "sqlite_leases", New: func() caddy.Module { return new(App) }}
}

var safeCN = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (a *App) Provision(ctx caddy.Context) error {
	if a.Database == "" {
		a.Database = "/var/lib/caddy/leases/leases.db"
	}
	if a.LeaseTTL == 0 {
		a.LeaseTTL = caddy.Duration(24 * time.Hour)
	}
	if a.CleanupInterval == nil {
		defaultInterval := caddy.Duration(10 * time.Minute)
		a.CleanupInterval = &defaultInterval
	}
	if a.PolicyURIPrefix == "" {
		a.PolicyURIPrefix = "urn:avalitan:caddy:lease:"
	}
	if a.AdminSocket == "" {
		a.AdminSocket = "/var/lib/caddy/.local/share/caddy/admin.sock"
	}
	if time.Duration(a.LeaseTTL) < time.Second || time.Duration(*a.CleanupInterval) < 0 {
		return errors.New("lease_ttl must be positive and cleanup_interval nonnegative")
	}
	for key, ttl := range a.LeasePolicies {
		if key == "" || time.Duration(ttl) < time.Second {
			return fmt.Errorf("invalid lease policy %q", key)
		}
	}
	for key, ttl := range a.HostOverrides {
		if !safeCN.MatchString(key) || time.Duration(ttl) < time.Second {
			return fmt.Errorf("invalid host override %q", key)
		}
	}
	if err := os.MkdirAll(filepath.Dir(a.Database), 0700); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", a.Database)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA busy_timeout=5000`, `PRAGMA journal_mode=WAL`,
		`CREATE TABLE IF NOT EXISTS leases (cn TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('l7','l4')), outer_id TEXT NOT NULL UNIQUE, policy TEXT NOT NULL, last_seen INTEGER NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY(cn,kind))`,
		`CREATE INDEX IF NOT EXISTS leases_expiration_idx ON leases(expires_at)`,
		`PRAGMA user_version=1`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return fmt.Errorf("initialize sqlite: %w", err)
		}
	}
	a.db = db
	a.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", a.AdminSocket)
	}}}
	if time.Duration(*a.CleanupInterval) > 0 {
		var workerCtx context.Context
		workerCtx, a.cancel = context.WithCancel(context.Background())
		a.done = make(chan struct{})
		go a.cleanupLoop(workerCtx)
	}
	return nil
}

func (a *App) Cleanup() error {
	if a.cancel != nil {
		a.cancel()
		<-a.done
	}
	if a.client != nil {
		a.client.CloseIdleConnections()
	}
	if a.db != nil {
		return a.db.Close()
	}
	return nil
}

func (a *App) cleanupLoop(ctx context.Context) {
	defer close(a.done)
	ticker := time.NewTicker(time.Duration(*a.CleanupInterval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = a.cleanupExpired(ctx) // failures are retried next interval
		}
	}
}

func (a *App) cleanupExpired(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows, err := a.db.QueryContext(ctx, `SELECT cn, kind, outer_id FROM leases WHERE expires_at <= unixepoch() ORDER BY expires_at`)
	if err != nil {
		return err
	}
	type expired struct{ cn, kind, outerID string }
	var items []expired
	for rows.Next() {
		var item expired
		if err := rows.Scan(&item.cn, &item.kind, &item.outerID); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var failures []error
	for _, item := range items {
		// Never accept a database-provided arbitrary ID as a deletion target.
		expected, err := routeID(item.cn, item.kind)
		if err != nil || expected != item.outerID {
			failures = append(failures, fmt.Errorf("invalid stored route for %q", item.cn))
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://localhost/id/"+expected, nil)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		resp, err := a.client.Do(req)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != 200 && resp.StatusCode != 204 && resp.StatusCode != 404 {
			failures = append(failures, fmt.Errorf("delete %s: HTTP %d", expected, resp.StatusCode))
			continue
		}
		_, err = a.db.ExecContext(ctx, `DELETE FROM leases WHERE cn=? AND kind=? AND outer_id=? AND expires_at <= unixepoch()`, item.cn, item.kind, item.outerID)
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func routeID(cn, kind string) (string, error) {
	if !safeCN.MatchString(cn) {
		return "", fmt.Errorf("unsafe certificate CN %q", cn)
	}
	switch kind {
	case "l7":
		return "site_auth_" + cn, nil
	case "l4":
		return "l4_auth_" + cn, nil
	default:
		return "", fmt.Errorf("invalid registration kind %q", kind)
	}
}

func (a *App) resolvePolicy(cn string, uris []string) (string, time.Duration, error) {
	if ttl, ok := a.HostOverrides[cn]; ok {
		return "override", time.Duration(ttl), nil
	}
	var policy string
	for _, uri := range uris {
		if !strings.HasPrefix(uri, a.PolicyURIPrefix) {
			continue
		}
		p := strings.TrimPrefix(uri, a.PolicyURIPrefix)
		if policy != "" {
			return "", 0, errors.New("multiple lease policy URI SANs")
		}
		if _, ok := a.LeasePolicies[p]; !ok {
			return "", 0, fmt.Errorf("unknown lease policy %q", p)
		}
		policy = p
	}
	if policy != "" {
		return policy, time.Duration(a.LeasePolicies[policy]), nil
	}
	return "default", time.Duration(a.LeaseTTL), nil
}

func (a *App) renew(ctx context.Context, cn, kind string, uris []string) error {
	id, err := routeID(cn, kind)
	if err != nil {
		return err
	}
	policy, ttl, err := a.resolvePolicy(cn, uris)
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO leases(cn,kind,outer_id,policy,last_seen,expires_at) VALUES(?,?,?,?,unixepoch(),unixepoch()+?) ON CONFLICT(cn,kind) DO UPDATE SET outer_id=excluded.outer_id,policy=excluded.policy,last_seen=excluded.last_seen,expires_at=excluded.expires_at`, cn, kind, id, policy, int64(ttl/time.Second))
	return err
}

var _ caddy.Provisioner = (*App)(nil)
var _ caddy.CleanerUpper = (*App)(nil)
