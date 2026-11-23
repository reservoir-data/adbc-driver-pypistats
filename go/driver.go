// Package pypistats implements a read-only ADBC driver that exposes the
// pypistats.org API as Arrow record streams.
//
// The statement "query" is a table name (python_minor or system). Each table
// returns one row per (package, category, date) for the configured packages.
package pypistats

import (
	"context"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/adbc-drivers/driverbase-go/driverbase"
	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

const (
	// OptionPackages is a comma-separated list of PyPI package names.
	// Settable on the database (default for all statements) and on statements.
	OptionPackages = "adbc.pypistats.packages"
	// OptionBaseURL overrides the API root (default https://pypistats.org/api).
	OptionBaseURL = "adbc.pypistats.base_url"
	// OptionMaxConcurrency bounds concurrent HTTP requests per query. Unset
	// (the default) picks a value automatically from the available CPUs.
	OptionMaxConcurrency = "adbc.pypistats.max_concurrency"
	// OptionMaxRetries is the number of retries on HTTP 429/5xx (default 3).
	OptionMaxRetries = "adbc.pypistats.max_retries"
	// OptionRequestTimeout is the per-request timeout in seconds (default 30).
	OptionRequestTimeout = "adbc.pypistats.request_timeout"

	// Requests are I/O-bound and pypistats.org rate-limits aggressively, so the
	// automatic concurrency is clamped to a small range.
	minAutoConcurrency = 2
	maxAutoConcurrency = 8

	defaultMaxRetries = 3
	defaultTimeout    = 30 * time.Second
	defaultBackoff    = time.Second
)

// config holds database-level settings; connections and statements copy it.
type config struct {
	packages       []string
	baseURL        string
	maxConcurrency int // 0 means detect automatically
	maxRetries     int
	timeout        time.Duration
}

func defaultConfig() config {
	return config{
		baseURL:    defaultBaseURL,
		maxRetries: defaultMaxRetries,
		timeout:    defaultTimeout,
	}
}

// effectiveMaxConcurrency resolves the configured concurrency, detecting a
// value from the available CPUs when it is unset.
func (c config) effectiveMaxConcurrency() int {
	if c.maxConcurrency > 0 {
		return c.maxConcurrency
	}
	return min(max(runtime.GOMAXPROCS(0), minAutoConcurrency), maxAutoConcurrency)
}

func (c config) newClient() *client {
	return &client{
		http:       &http.Client{Timeout: c.timeout},
		baseURL:    c.baseURL,
		userAgent:  defaultUserAgent,
		maxRetries: c.maxRetries,
		backoff:    defaultBackoff,
	}
}

func parsePackages(v string) []string {
	var out []string
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---- Driver ----

type driverImpl struct {
	driverbase.DriverImplBase
}

// NewDriver constructs the ADBC driver for pypistats.
func NewDriver(alloc memory.Allocator) driverbase.DriverWithContext {
	info := driverbase.DefaultDriverInfo("PyPIStats")
	info.MustRegister(map[adbc.InfoCode]any{
		adbc.InfoDriverName: "ADBC Driver for pypistats.org",
		adbc.InfoVendorSql:  false,
	})
	return driverbase.NewDriver(&driverImpl{
		DriverImplBase: driverbase.NewDriverImplBase(info, alloc),
	})
}

func (d *driverImpl) NewDatabaseWithContext(ctx context.Context, opts map[string]string) (adbc.DatabaseWithContext, error) {
	base, err := driverbase.NewDatabaseImplBase(ctx, &d.DriverImplBase, driverbase.TracingOptions{})
	if err != nil {
		return nil, err
	}
	db := &databaseImpl{DatabaseImplBase: base, cfg: defaultConfig()}
	if err := db.SetOptions(ctx, opts); err != nil {
		return nil, err
	}
	return driverbase.NewDatabase(db), nil
}

// ---- Database ----

type databaseImpl struct {
	driverbase.DatabaseImplBase
	cfg config
}

func (db *databaseImpl) SetOptions(ctx context.Context, opts map[string]string) error {
	for k, v := range opts {
		if err := db.SetOption(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (db *databaseImpl) SetOption(ctx context.Context, key, val string) error {
	switch key {
	case OptionPackages:
		db.cfg.packages = parsePackages(val)
	case OptionBaseURL:
		if val == "" {
			return db.ErrorHelper.InvalidArgument("%s must not be empty", key)
		}
		db.cfg.baseURL = val
	case OptionMaxConcurrency, OptionMaxRetries:
		if key == OptionMaxConcurrency && val == "" {
			db.cfg.maxConcurrency = 0 // back to automatic
			return nil
		}
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return db.ErrorHelper.InvalidArgument("%s must be an integer, got %q", key, val)
		}
		return db.SetOptionInt(ctx, key, n)
	case OptionRequestTimeout:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return db.ErrorHelper.InvalidArgument("%s must be a number of seconds, got %q", key, val)
		}
		return db.SetOptionDouble(ctx, key, f)
	case adbc.OptionKeyURI:
		// Nothing to connect to; accept an empty or pypistats:// URI so the
		// driver works with tools that always pass one.
		if val != "" && !strings.HasPrefix(val, "pypistats://") {
			return db.ErrorHelper.InvalidArgument("unsupported uri %q (expected empty or pypistats://)", val)
		}
	default:
		return db.DatabaseImplBase.SetOption(ctx, key, val)
	}
	return nil
}

func (db *databaseImpl) SetOptionInt(ctx context.Context, key string, val int64) error {
	switch key {
	case OptionMaxConcurrency:
		if val < 1 {
			return db.ErrorHelper.InvalidArgument("%s must be >= 1, got %d", key, val)
		}
		db.cfg.maxConcurrency = int(val)
	case OptionMaxRetries:
		if val < 0 {
			return db.ErrorHelper.InvalidArgument("%s must be >= 0, got %d", key, val)
		}
		db.cfg.maxRetries = int(val)
	case OptionRequestTimeout:
		return db.SetOptionDouble(ctx, key, float64(val))
	default:
		return db.DatabaseImplBase.SetOptionInt(ctx, key, val)
	}
	return nil
}

func (db *databaseImpl) SetOptionDouble(ctx context.Context, key string, val float64) error {
	switch key {
	case OptionRequestTimeout:
		if val <= 0 {
			return db.ErrorHelper.InvalidArgument("%s must be > 0, got %v", key, val)
		}
		db.cfg.timeout = time.Duration(val * float64(time.Second))
	default:
		return db.DatabaseImplBase.SetOptionDouble(ctx, key, val)
	}
	return nil
}

func (db *databaseImpl) GetOption(ctx context.Context, key string) (string, error) {
	switch key {
	case OptionPackages:
		return strings.Join(db.cfg.packages, ","), nil
	case OptionBaseURL:
		return db.cfg.baseURL, nil
	case OptionMaxConcurrency:
		if db.cfg.maxConcurrency == 0 {
			return "", nil
		}
		return strconv.Itoa(db.cfg.maxConcurrency), nil
	case OptionMaxRetries:
		return strconv.Itoa(db.cfg.maxRetries), nil
	case OptionRequestTimeout:
		return strconv.FormatFloat(db.cfg.timeout.Seconds(), 'f', -1, 64), nil
	}
	return db.DatabaseImplBase.GetOption(ctx, key)
}

func (db *databaseImpl) GetOptionInt(ctx context.Context, key string) (int64, error) {
	switch key {
	case OptionMaxConcurrency:
		return int64(db.cfg.effectiveMaxConcurrency()), nil
	case OptionMaxRetries:
		return int64(db.cfg.maxRetries), nil
	}
	return db.DatabaseImplBase.GetOptionInt(ctx, key)
}

func (db *databaseImpl) GetOptionDouble(ctx context.Context, key string) (float64, error) {
	if key == OptionRequestTimeout {
		return db.cfg.timeout.Seconds(), nil
	}
	return db.DatabaseImplBase.GetOptionDouble(ctx, key)
}

func (db *databaseImpl) Open(ctx context.Context) (adbc.ConnectionWithContext, error) {
	cfg := db.cfg
	cfg.packages = append([]string(nil), cfg.packages...)
	conn := &connectionImpl{
		ConnectionImplBase: driverbase.NewConnectionImplBase(&db.DatabaseImplBase),
		cfg:                cfg,
		client:             cfg.newClient(),
	}
	return driverbase.NewConnectionBuilder(conn).
		WithDbObjectsEnumerator(conn).
		WithTableTypeLister(conn).
		WithAutocommitSetter(conn).
		Connection(), nil
}
