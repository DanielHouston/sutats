// Package db is the TimescaleDB backend: every status sample is a row in a
// hypertable, day buckets for the 90-day bars come from a continuous
// aggregate, and an advisory lock elects the one replica that writes.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DanielHouston/sutats/internal/history"
	"github.com/DanielHouston/sutats/internal/model"
	"github.com/DanielHouston/sutats/internal/probe"
)

// Advisory lock IDs. Arbitrary but fixed.
const (
	leaderLockID  = 7340101
	migrateLockID = 7340102
)

// DB wraps a connection pool.
type DB struct {
	pool *pgxpool.Pool

	mu       sync.Mutex
	lastSeen map[string]time.Time
}

// Open connects and pings. url is a libpq/pgx connection string.
func Open(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parsing database url: %w", err)
	}
	cfg.MaxConns = 6
	cfg.ConnConfig.RuntimeParams["application_name"] = "sutats"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	if err := waitForDatabase(ctx, pool, StartupWait); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{pool: pool, lastSeen: map[string]time.Time{}}, nil
}

// StartupWait is how long Open keeps trying a database that cannot be
// reached yet. Databases often start alongside sutats (Compose, a Pod's
// sibling container) and refuse connections for a while: Postgres images
// restart the server once after first-time setup.
var StartupWait = 2 * time.Minute

// waitForDatabase pings until the database answers. Connection failures
// are retried with growing pauses until wait runs out; an error from the
// server itself, such as a wrong password or a missing database, will not
// fix itself and fails at once.
func waitForDatabase(ctx context.Context, pool *pgxpool.Pool, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	pause := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := pool.Ping(pingCtx)
		cancel()
		if err == nil {
			if attempt > 1 {
				slog.Info("database is reachable", "attempts", attempt)
			}
			return nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) || ctx.Err() != nil || time.Now().Add(pause).After(deadline) {
			return fmt.Errorf("pinging database: %w", err)
		}
		if attempt == 1 {
			slog.Info("waiting for the database", "err", err, "for_up_to", wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
		pause = min(pause*2, 5*time.Second)
	}
}

// Close releases the pool.
func (d *DB) Close() { d.pool.Close() }

// schemaVersion is bumped when a step in versioned must run once on existing
// databases (for example recreating the day aggregate).
const schemaVersion = 2

// migrations run in order under an advisory lock on every start; each
// statement is idempotent so any replica can run them.
var migrations = []string{
	`CREATE EXTENSION IF NOT EXISTS timescaledb`,
	`CREATE TABLE IF NOT EXISTS samples (
		time        timestamptz NOT NULL,
		key         text        NOT NULL,
		status      smallint    NOT NULL,
		elapsed     real        NOT NULL,
		latency_ms  integer
	)`,
	`SELECT create_hypertable('samples', 'time', if_not_exists => TRUE)`,
	`CREATE INDEX IF NOT EXISTS samples_key_time ON samples (key, time DESC)`,
	`ALTER TABLE samples SET (timescaledb.compress, timescaledb.compress_segmentby = 'key', timescaledb.compress_orderby = 'time DESC')`,
	// sutats only appends, so there is no reason to hold raw rows for long:
	// compress after a day. Remove-then-add updates existing installs.
	`SELECT remove_compression_policy('samples', if_exists => TRUE)`,
	`SELECT add_compression_policy('samples', INTERVAL '1 day')`,
	`SELECT add_retention_policy('samples', INTERVAL '100 days', if_not_exists => TRUE)`,
	`CREATE TABLE IF NOT EXISTS sutats_meta (key text PRIMARY KEY, value text NOT NULL)`,
	// Day buckets: worst status that lasted some time, seconds observed,
	// seconds degraded or down. materialized_only = false blends in rows
	// newer than the last refresh, so today is always current.
	`CREATE MATERIALIZED VIEW IF NOT EXISTS samples_daily
		WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
		SELECT time_bucket('1 day', time) AS day, key,
		       coalesce(max(status) FILTER (WHERE elapsed > 0), 0) AS status,
		       sum(elapsed)                                        AS observed,
		       coalesce(sum(elapsed) FILTER (WHERE status >= 3), 0) AS downtime
		FROM samples GROUP BY day, key WITH NO DATA`,
	// Refresh the whole retention window, not just recent days, so backfilled
	// or late rows always land in a bucket. Timescale only recomputes buckets
	// whose raw rows changed, so this stays cheap. Remove-then-add keeps the
	// policy in step with this code on upgrade.
	`SELECT remove_continuous_aggregate_policy('samples_daily', if_exists => TRUE)`,
	`SELECT add_continuous_aggregate_policy('samples_daily',
		start_offset => INTERVAL '100 days', end_offset => INTERVAL '1 hour',
		schedule_interval => INTERVAL '15 minutes')`,
	`CREATE TABLE IF NOT EXISTS probe_state (
		key         text PRIMARY KEY,
		status      smallint NOT NULL,
		detail      text     NOT NULL,
		latency_ms  bigint   NOT NULL,
		failures    integer  NOT NULL,
		updated_at  timestamptz NOT NULL DEFAULT now()
	)`,
}

// versioned are one-off steps keyed by the schema version they bring the
// database to. They run only when the stored version is lower. Each runs
// before the idempotent migrations, which then recreate what was dropped.
var versioned = map[int][]string{
	// v2: the day aggregate ignores zero-length observations when picking a
	// day's status. A continuous aggregate cannot be altered, so drop it and
	// let the idempotent list recreate it; Migrate refreshes it afterwards.
	2: {`DROP MATERIALIZED VIEW IF EXISTS samples_daily`},
}

// Migrate creates or updates the schema. Safe to run from every replica.
func (d *DB) Migrate(ctx context.Context) error {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockID); err != nil {
		return fmt.Errorf("taking migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrateLockID) }()

	// Find the stored version. A database created before versioning that
	// already has the aggregate is treated as version 1.
	current := 0
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('sutats_meta') IS NOT NULL`).Scan(&exists); err == nil && exists {
		var v string
		if err := conn.QueryRow(ctx, `SELECT value FROM sutats_meta WHERE key = 'schema_version'`).Scan(&v); err == nil {
			fmt.Sscanf(v, "%d", &current)
		}
	} else if err := conn.QueryRow(ctx, `SELECT to_regclass('samples_daily') IS NOT NULL`).Scan(&exists); err == nil && exists {
		current = 1
	}

	recreated := false
	for v := current + 1; v <= schemaVersion; v++ {
		for i, stmt := range versioned[v] {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("migration to v%d step %d: %w", v, i, err)
			}
			recreated = true
		}
	}
	for i, stmt := range migrations {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO sutats_meta (key, value) VALUES ('schema_version', $1)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, fmt.Sprint(schemaVersion)); err != nil {
		return fmt.Errorf("recording schema version: %w", err)
	}
	if recreated && current > 0 {
		slog.Info("day aggregate recreated, rebuilding from samples", "from_version", current, "to_version", schemaVersion)
		if _, err := conn.Exec(ctx, `CALL refresh_continuous_aggregate('samples_daily', NULL, NULL)`); err != nil {
			return fmt.Errorf("rebuilding day aggregate: %w", err)
		}
	}
	return nil
}

// Record writes one row per component and per signal. The time since the
// previous sample for each key is attributed to its current status, capped
// at maxGap, exactly as the in-memory store does.
func (d *DB) Record(ctx context.Context, now time.Time, comps []model.Component, maxGap time.Duration) error {
	d.mu.Lock()
	now = now.UTC()
	type row struct {
		key     string
		status  model.Status
		latency *int64
	}
	var rows []row
	seen := map[string]bool{}
	for _, c := range comps {
		rows = append(rows, row{c.Key(), c.Status, nil})
		seen[c.Key()] = true
		for _, sig := range c.Signals {
			if !sig.HasBar() {
				continue
			}
			k := history.SignalKey(c.Key(), sig.Name)
			seen[k] = true
			var lat *int64
			if sig.Kind == model.SignalHTTP && sig.LatencyMS > 0 {
				l := sig.LatencyMS
				lat = &l
			}
			rows = append(rows, row{k, sig.Status, lat})
		}
	}
	values := make([][]any, 0, len(rows))
	for _, r := range rows {
		last, ok := d.lastSeen[r.key]
		before, after := history.SplitGap(now, history.Gap(now, last, ok, maxGap))
		d.lastSeen[r.key] = now
		// The part of the gap before midnight goes in the day before's
		// bucket, stamped just ahead of midnight.
		if before > 0 {
			values = append(values, []any{now.Add(-after - time.Microsecond), r.key, int16(r.status), float32(before.Seconds()), nil})
		}
		values = append(values, []any{now, r.key, int16(r.status), float32(after.Seconds()), r.latency})
	}
	for k := range d.lastSeen {
		if !seen[k] {
			delete(d.lastSeen, k)
		}
	}
	d.mu.Unlock()

	if len(values) == 0 {
		return nil
	}
	_, err := d.pool.CopyFrom(ctx, pgx.Identifier{"samples"},
		[]string{"time", "key", "status", "elapsed", "latency_ms"}, pgx.CopyFromRows(values))
	if err != nil {
		return fmt.Errorf("writing samples: %w", err)
	}
	return nil
}

// LoadDays replaces the store's contents with the last n days from the
// continuous aggregate. Every replica calls this on a timer.
func (d *DB) LoadDays(ctx context.Context, store *history.Store, n int, now time.Time) error {
	since := now.UTC().AddDate(0, 0, -n+1).Truncate(24 * time.Hour)
	rows, err := d.pool.Query(ctx,
		`SELECT key, day, status, observed, downtime FROM samples_daily WHERE day >= $1 ORDER BY key, day`, since)
	if err != nil {
		return fmt.Errorf("reading day buckets: %w", err)
	}
	defer rows.Close()
	all := map[string][]model.Day{}
	for rows.Next() {
		var (
			key      string
			day      time.Time
			status   int16
			observed float64
			downtime float64
		)
		if err := rows.Scan(&key, &day, &status, &observed, &downtime); err != nil {
			return err
		}
		all[key] = append(all[key], model.Day{
			Date: day.UTC().Format("2006-01-02"), Status: model.Status(status), Observed: observed, Downtime: downtime,
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	store.ReplaceAll(all)
	return nil
}

// SaveLive upserts the latest probe results.
func (d *DB) SaveLive(ctx context.Context, states map[string]probe.State) error {
	batch := &pgx.Batch{}
	for k, st := range states {
		batch.Queue(`INSERT INTO probe_state (key, status, detail, latency_ms, failures, updated_at)
			VALUES ($1, $2, $3, $4, $5, now())
			ON CONFLICT (key) DO UPDATE SET status = EXCLUDED.status, detail = EXCLUDED.detail,
			latency_ms = EXCLUDED.latency_ms, failures = EXCLUDED.failures, updated_at = now()`,
			k, int16(st.Status), st.Detail, st.LatencyMS, st.Failures)
	}
	if batch.Len() == 0 {
		return nil
	}
	res := d.pool.SendBatch(ctx, batch)
	defer res.Close()
	for i := 0; i < batch.Len(); i++ {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("saving probe state: %w", err)
		}
	}
	return nil
}

// LoadLive reads the latest probe results, ignoring anything stale.
func (d *DB) LoadLive(ctx context.Context, maxAge time.Duration) (map[string]probe.State, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT key, status, detail, latency_ms, failures FROM probe_state WHERE updated_at > now() - $1::interval`,
		maxAge.String())
	if err != nil {
		return nil, fmt.Errorf("reading probe state: %w", err)
	}
	defer rows.Close()
	out := map[string]probe.State{}
	for rows.Next() {
		var (
			k      string
			status int16
			st     probe.State
		)
		if err := rows.Scan(&k, &status, &st.Detail, &st.LatencyMS, &st.Failures); err != nil {
			return nil, err
		}
		st.Status = model.Status(status)
		out[k] = st
	}
	return out, rows.Err()
}

// Bucket is one point of a fine-grained series.
type Bucket struct {
	Time      time.Time    `json:"time"`
	Status    model.Status `json:"status"`
	LatencyMS *float64     `json:"latency_ms,omitempty"`
	Observed  float64      `json:"observed_seconds"`
	Downtime  float64      `json:"downtime_seconds"`
}

// Series returns time buckets for one key between two instants. This is the
// fine-grained view the hypertable exists for.
func (d *DB) Series(ctx context.Context, key string, from, to time.Time, bucket time.Duration) ([]Bucket, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT time_bucket($4::interval, time) AS t, max(status), avg(latency_ms),
		        sum(elapsed), coalesce(sum(elapsed) FILTER (WHERE status >= 3), 0)
		 FROM samples WHERE key = $1 AND time >= $2 AND time < $3
		 GROUP BY t ORDER BY t`, key, from.UTC(), to.UTC(), bucket.String())
	if err != nil {
		return nil, fmt.Errorf("reading series: %w", err)
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var (
			b      Bucket
			status int16
			lat    *float64
		)
		if err := rows.Scan(&b.Time, &status, &lat, &b.Observed, &b.Downtime); err != nil {
			return nil, err
		}
		b.Status = model.Status(status)
		b.LatencyMS = lat
		out = append(out, b)
	}
	return out, rows.Err()
}

// Earliest returns the time of the oldest sample, or ok=false if none.
func (d *DB) Earliest(ctx context.Context) (t time.Time, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `SELECT min(time) FROM samples`).Scan(&t)
	if err != nil || t.IsZero() {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// Empty reports whether no samples exist yet (used to decide on seeding).
func (d *DB) Empty(ctx context.Context) (bool, error) {
	var one int
	err := d.pool.QueryRow(ctx, `SELECT 1 FROM samples LIMIT 1`).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return false, err
}

// SeedSeries is one signal to generate samples for.
type SeedSeries struct {
	Key       string
	Component string // the component's key; gets worst-of-signals rows
	HTTP      bool   // emit latency
	BaseMS    int    // typical latency when healthy
}

// SeedWindow marks a span during which a key was in a given status.
type SeedWindow struct {
	Key        string
	Start, End time.Time
	Status     model.Status
}

// SeedSamples generates one sample per step for every series between from
// and to, entirely inside the database: operational outside the windows,
// the window's status inside, with latency that drifts gently through the
// day and spikes while degraded. Component rows are the worst of their
// signals at each step. Mock mode only.
func (d *DB) SeedSamples(ctx context.Context, series []SeedSeries, windows []SeedWindow, from, to time.Time, step time.Duration) error {
	if len(series) == 0 {
		return nil
	}
	var (
		keys, comps []string
		http        []bool
		base        []int32
	)
	for _, s := range series {
		keys = append(keys, s.Key)
		comps = append(comps, s.Component)
		http = append(http, s.HTTP)
		base = append(base, int32(s.BaseMS))
	}
	var (
		wKeys          []string
		wStart, wEnd   []time.Time
		wStatus        []int16
		elapsedSeconds = float32(step.Seconds())
	)
	for _, w := range windows {
		wKeys = append(wKeys, w.Key)
		wStart = append(wStart, w.Start.UTC())
		wEnd = append(wEnd, w.End.UTC())
		wStatus = append(wStatus, int16(w.Status))
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		WITH specs AS (
			SELECT * FROM unnest($1::text[], $2::bool[], $3::int[]) AS s(key, http, base_ms)
		), wins AS (
			SELECT * FROM unnest($4::text[], $5::timestamptz[], $6::timestamptz[], $7::smallint[]) AS w(key, ws, we, status)
		), ticks AS (
			SELECT generate_series($8::timestamptz, $9::timestamptz, $10::interval) AS t
		), graded AS (
			SELECT t, s.key, s.http, s.base_ms,
			       coalesce((SELECT max(w.status) FROM wins w WHERE w.key = s.key AND t >= w.ws AND t < w.we), 1) AS status
			FROM ticks, specs s
		)
		INSERT INTO samples (time, key, status, elapsed, latency_ms)
		SELECT t, key, status, $11,
		       CASE
		         WHEN NOT http OR status = 4 THEN NULL
		         WHEN status = 3 THEN (base_ms * (10 + random() * 8))::int
		         ELSE (base_ms * (0.85 + random() * 0.3 + 0.12 * sin(extract(epoch FROM t) / 5400.0)))::int
		       END
		FROM graded`,
		keys, http, base, wKeys, wStart, wEnd, wStatus, from.UTC(), to.UTC(), step.String(), elapsedSeconds)
	if err != nil {
		return fmt.Errorf("seeding signal samples: %w", err)
	}

	_, err = tx.Exec(ctx, `
		WITH owner AS (
			SELECT * FROM unnest($1::text[], $2::text[]) AS o(key, component)
		)
		INSERT INTO samples (time, key, status, elapsed, latency_ms)
		SELECT s.time, o.component, max(s.status), $5, NULL
		FROM samples s JOIN owner o ON o.key = s.key
		WHERE s.time >= $3 AND s.time <= $4
		GROUP BY s.time, o.component`,
		keys, comps, from.UTC(), to.UTC(), elapsedSeconds)
	if err != nil {
		return fmt.Errorf("seeding component samples: %w", err)
	}
	return tx.Commit(ctx)
}

// Compress compresses every chunk older than the compression delay now,
// instead of waiting for the policy's next run. Used after seeding.
func (d *DB) Compress(ctx context.Context) error {
	_, err := d.pool.Exec(ctx,
		`SELECT compress_chunk(c, if_not_compressed => TRUE) FROM show_chunks('samples', older_than => INTERVAL '1 day') c`)
	if err != nil {
		return fmt.Errorf("compressing chunks: %w", err)
	}
	return nil
}

// Refresh materialises the day aggregate over all time. Call it after
// writing historical rows (seeding, imports); normal sampling does not need
// it because the policy and real-time aggregation cover recent data.
func (d *DB) Refresh(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `CALL refresh_continuous_aggregate('samples_daily', NULL, NULL)`)
	if err != nil {
		return fmt.Errorf("refreshing day aggregate: %w", err)
	}
	return nil
}

// Lead runs run(ctx) while this process holds the leader advisory lock. It
// retries forever: waiting for the lock when another replica has it, and
// re-trying after a lost connection. The lock is released when the
// connection holding it closes, so a dead leader frees it automatically.
func (d *DB) Lead(ctx context.Context, run func(ctx context.Context)) {
	for ctx.Err() == nil {
		conn, err := d.pool.Acquire(ctx)
		if err != nil {
			slog.Warn("leader: cannot get a connection", "err", err)
			sleep(ctx, 5*time.Second)
			continue
		}
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leaderLockID).Scan(&got); err != nil || !got {
			conn.Release()
			sleep(ctx, 5*time.Second)
			continue
		}
		slog.Info("became leader; sampling and writing to the database")
		lctx, cancel := context.WithCancel(ctx)
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-lctx.Done():
					return
				case <-t.C:
					if err := conn.Ping(lctx); err != nil {
						slog.Warn("leader: lost database connection, giving up leadership", "err", err)
						cancel()
						return
					}
				}
			}
		}()
		run(lctx)
		cancel()
		// Close the connection rather than returning it to the pool, so the
		// lock is dropped with it.
		_ = conn.Conn().Close(context.Background())
		conn.Release()
		if ctx.Err() == nil {
			slog.Info("no longer leader")
			sleep(ctx, time.Second)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
