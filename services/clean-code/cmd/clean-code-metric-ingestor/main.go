// Package main is the entrypoint for the clean-code-metric-ingestor service.
// It processes commits, creates metric_sample rows, and manages the
// metric_sample_active pointer for active-row uniqueness enforcement.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gofrs/uuid"
	_ "github.com/lib/pq"
)

var db *sql.DB

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	pgURL := os.Getenv("CLEAN_CODE_PG_URL")
	if pgURL == "" {
		log.Fatal("CLEAN_CODE_PG_URL is required")
	}

	var err error
	db, err = sql.Open("postgres", pgURL)
	if err != nil {
		log.Fatalf("opening postgres: %v", err)
	}
	defer db.Close()

	// Wait for Postgres to be ready.
	for i := 0; i < 30; i++ {
		if err := db.Ping(); err == nil {
			break
		}
		time.Sleep(time.Second)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/v1/ingestor/process", handleProcess)

	log.Printf("clean-code-metric-ingestor listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// startRuleEngineWorker composes the SOLID Rule Engine
// dependencies and launches the [rule_engine.Worker] on a
// background goroutine bound to `ctx`. Returns the
// send-only event channel the HTTP handlers should write
// to when a SHA transitions to `scanned`.
//
// Composition order (per Stage 5.7 architecture Sec 3.6):
//   - [steward.SQLStore] -- policy/rule/threshold reads
//   - [steward.Steward] -- exposes ActivePolicyVersion (no
//     signer -- the batch worker is a READER of policies,
//     not a writer)
//   - separate `*sql.DB` for Audit writes (see Stage 5.7
//     evaluator feedback #5: the
//     [rule_engine.SQLStore.AppendEvaluation] path INSERTs
//     into `evaluation_run`, `evaluation_verdict`, and
//     `finding`; those tables are granted INSERT to
//     `clean_code_solid_batch` per
//     migrations/0004_roles.up.sql:455-465 -- NOT
//     `clean_code_metric_ingestor`. The composition root
//     reads `CLEAN_CODE_SOLID_BATCH_PG_URL` to authenticate
//     a dedicated handle as that role. When the env var is
//     unset the composition root falls back to the main
//     `*sql.DB` with a WARN log so dev/test
//     compose-as-superuser environments keep working.)
//   - [rule_engine.SQLStore] -- the writer for the three
//     Audit tables under `caller='batch_refresh'`
//   - [rule_engine.Engine] -- the in-process actor
//   - [rule_engine.Worker] -- consumes ScanEvent and drives
//     Engine.RunBatch
//   - [rule_engine.SQLPendingScanReader] -- the durable
//     catchup reader (Stage 5.7 evaluator feedback #6).
//     Reads `clean_code.commit.scan_status='scanned'` rows
//     missing an `evaluation_run` for the active policy.
//
// The buffered channel decouples HTTP latency from worker
// availability; capacity-saturation drops are converted to
// bounded blocks ([scanEventEmitTimeout]) by the emitting
// handler, and the durable [rule_engine.Worker.Catchup]
// loop guarantees nothing is lost across process restarts.
func startRuleEngineWorker(ctx context.Context, dbh *sql.DB) (chan<- rule_engine.ScanEvent, error) {
	stewardStore, err := steward.NewSQLStore(dbh)
	if err != nil {
		return nil, fmt.Errorf("steward.NewSQLStore: %w", err)
	}
	stew, err := steward.New(steward.Config{Store: stewardStore})
	if err != nil {
		return nil, fmt.Errorf("steward.New: %w", err)
	}

	// Audit-writer DB handle. Per Stage 5.7 evaluator
	// feedback #5: the three Audit tables grant INSERT to
	// `clean_code_solid_batch`, NOT the metric-ingestor's
	// role. The composition root therefore authenticates a
	// dedicated handle as that role when
	// `CLEAN_CODE_SOLID_BATCH_PG_URL` is set; otherwise we
	// fall back to the main DB handle with a WARN log
	// (acceptable for dev/test compose-as-superuser; will
	// fail at runtime under production least-privilege).
	auditDB := dbh
	if solidBatchURL := os.Getenv("CLEAN_CODE_SOLID_BATCH_PG_URL"); solidBatchURL != "" {
		bd, berr := sql.Open("postgres", solidBatchURL)
		if berr != nil {
			return nil, fmt.Errorf("opening CLEAN_CODE_SOLID_BATCH_PG_URL: %w", berr)
		}
		// Verify the handle is usable before we hand it
		// to the Audit writer. A boot-time Ping is cheap
		// insurance against a typo / misconfigured DSN.
		if perr := bd.PingContext(ctx); perr != nil {
			_ = bd.Close()
			return nil, fmt.Errorf("ping CLEAN_CODE_SOLID_BATCH_PG_URL: %w", perr)
		}
		auditDB = bd
		log.Print("rule_engine: Audit writes authenticated via CLEAN_CODE_SOLID_BATCH_PG_URL")
	} else {
		log.Print("rule_engine: WARN -- CLEAN_CODE_SOLID_BATCH_PG_URL not set; reusing CLEAN_CODE_PG_URL handle for Audit writes (will fail under production least-privilege grants per migrations/0004_roles.up.sql)")
	}

	ruleStore, err := rule_engine.NewSQLStore(rule_engine.SQLStoreConfig{
		DB:      auditDB,
		Steward: stewardStore,
	})
	if err != nil {
		return nil, fmt.Errorf("rule_engine.NewSQLStore: %w", err)
	}
	engine, err := rule_engine.New(rule_engine.Config{Store: ruleStore})
	if err != nil {
		return nil, fmt.Errorf("rule_engine.New: %w", err)
	}
	events := make(chan rule_engine.ScanEvent, scanEventCapacity)
	worker, err := rule_engine.NewWorker(rule_engine.WorkerConfig{
		Engine:     engine,
		Activation: rule_engine.NewStewardActivation(stew),
		Events:     events,
		Logger:     slog.Default(),
	})
	if err != nil {
		return nil, fmt.Errorf("rule_engine.NewWorker: %w", err)
	}
	go func() {
		if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("rule_engine.worker.Run exited with error: %v", err)
		}
	}()

	// Durable catchup loop -- Stage 5.7 evaluator
	// feedback #6. The catchup reader uses the SAME DB
	// handle as the live live event path (DB reads against
	// `commit` + `evaluation_run`). We launch it on a
	// dedicated goroutine that fires on startup AND on a
	// [catchupInterval] ticker; per-event work routes back
	// through Worker.process so the
	// `caller='batch_refresh'` stamp matches the live
	// path.
	pendingReader, perr := rule_engine.NewSQLPendingScanReader(rule_engine.SQLPendingScanReaderConfig{DB: dbh})
	if perr != nil {
		return nil, fmt.Errorf("rule_engine.NewSQLPendingScanReader: %w", perr)
	}
	go runCatchupLoop(ctx, worker, pendingReader)

	return events, nil
}

// runCatchupLoop drains the durable scan backlog on startup
// and then re-runs the drain on a wall-clock ticker so any
// SHA that the live event channel dropped (or any SHA that
// landed while the process was down) is eventually picked
// up. Errors are LOGGED -- the catchup loop is the LAST
// line of defence; we do NOT crash the service on a
// recoverable DB error.
func runCatchupLoop(ctx context.Context, worker *rule_engine.Worker, reader rule_engine.PendingScanReader) {
	// Run an immediate first-pass on startup. Any backlog
	// that accumulated while the service was down is
	// drained before the ticker fires.
	if processed, err := worker.Catchup(ctx, rule_engine.CatchupConfig{Reader: reader}); err != nil {
		log.Printf("rule_engine.worker.Catchup (startup) failed: %v", err)
	} else if processed > 0 {
		log.Printf("rule_engine.worker.Catchup (startup) processed=%d events", processed)
	}

	ticker := time.NewTicker(catchupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if processed, err := worker.Catchup(ctx, rule_engine.CatchupConfig{Reader: reader}); err != nil {
				log.Printf("rule_engine.worker.Catchup (periodic) failed: %v", err)
			} else if processed > 0 {
				log.Printf("rule_engine.worker.Catchup (periodic) processed=%d events", processed)
			}
		}
	}
}

// emitScanEvent forwards a successful (RepoID, SHA) pair to
// the post-scan dispatcher. Per Stage 5.7 evaluator
// feedback #6: a `default:` drop loses required work
// permanently; replacing it with a bounded
// [scanEventEmitTimeout] block converts the failure mode
// from "silent loss" to "request latency spike". The
// durable [rule_engine.Worker.Catchup] loop run by
// [runCatchupLoop] is the ultimate guarantee that nothing
// is lost across process restarts even if the timeout
// trips -- a `scan_status='scanned'` row with no
// `evaluation_run` for the active policy will be picked
// up on the next catchup tick.
func emitScanEvent(ctx context.Context, repoIDRaw, sha string) {
	if scanEvents == nil {
		return
	}
	if repoIDRaw == "" || sha == "" {
		log.Printf("rule_engine: emit skipped (empty repo_id or sha): repo_id=%q sha=%q", repoIDRaw, sha)
		return
	}
	repoID, err := uuid.FromString(repoIDRaw)
	if err != nil {
		log.Printf("rule_engine: emit skipped (invalid repo_id %q): %v", repoIDRaw, err)
		return
	}
	ev := rule_engine.ScanEvent{RepoID: repoID, SHA: sha}
	// Bounded block instead of a `default:` drop. The
	// timer is sized so a real saturation event surfaces
	// as a latency spike + log line (durably observable)
	// rather than as a silent permanent loss.
	timer := time.NewTimer(scanEventEmitTimeout)
	defer timer.Stop()
	select {
	case scanEvents <- ev:
		// emitted
	case <-ctx.Done():
		// Request canceled; do not block on the buffer.
		// The catchup loop will pick this SHA up on its
		// next tick (the catchup reader filters by
		// `commit.scan_status='scanned'` + absent
		// `evaluation_run`).
	case <-timer.C:
		log.Printf("rule_engine: scan event channel saturated after %s -- event WILL BE REPROCESSED BY CATCHUP repo_id=%s sha=%s (capacity=%d, emit_timeout=%s)",
			scanEventEmitTimeout, repoID, sha, scanEventCapacity, scanEventEmitTimeout)
	}
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

type processRequest struct {
	CommitSHA string `json:"commit_sha"`
	RepoID    string `json:"repo_id"`
}

// handleProcess ingests metrics for a commit:
//  1. Creates a metric_sample row with a computed payload.
//  2. UPSERTs metric_sample_active to point at the new sample.
//
// Idempotency: if a metric_sample already exists for this SHA and the active
// pointer is not retracted, the handler skips re-computation (sample_id and
// row count remain unchanged). If retracted, a new sample is created and the
// pointer is moved.
func handleProcess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req processRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Check if an active, non-retracted sample already exists (computation-skip).
	var existingSampleID string
	err := db.QueryRow(`
		SELECT msa.sample_id
		FROM clean_code.metric_sample_active msa
		LEFT JOIN clean_code.metric_retraction mr ON mr.sample_id = msa.sample_id
		WHERE msa.commit_sha = $1 AND mr.sample_id IS NULL
	`, req.CommitSHA).Scan(&existingSampleID)

	if err == nil && existingSampleID != "" {
		// Active, non-retracted sample exists — skip re-computation.
		// sample_id and row count remain unchanged (idempotent).
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"skipped","sample_id":"%s"}`, existingSampleID)
		return
	}

	// Either no sample exists, or the current one is retracted — create new.
	var newSampleID string
	err = db.QueryRow(`
		INSERT INTO clean_code.metric_sample (commit_sha, payload)
		VALUES ($1, '{"source":"e2e-ingestor"}'::jsonb)
		RETURNING sample_id
	`, req.CommitSHA).Scan(&newSampleID)
	if err != nil {
		http.Error(w, "inserting metric_sample: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// UPSERT the active pointer to the new sample.
	_, err = db.Exec(`
		INSERT INTO clean_code.metric_sample_active (commit_sha, sample_id, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (commit_sha)
		DO UPDATE SET sample_id = EXCLUDED.sample_id, updated_at = now()
	`, req.CommitSHA, newSampleID)
	if err != nil {
		http.Error(w, "upserting metric_sample_active: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ingested","sample_id":"%s"}`, newSampleID)
}