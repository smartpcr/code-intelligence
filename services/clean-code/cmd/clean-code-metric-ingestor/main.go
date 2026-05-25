// Package main is the entrypoint for the clean-code-metric-ingestor service.
// It processes commits through scan recipes and manages the ScanRun lifecycle.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// validScanRunKinds enumerates the allowed scan_run kind values.
// The ingestor MUST reject any kind not in this set before reaching PostgreSQL.
var validScanRunKinds = map[string]bool{
	"ast_metrics": true,
	"lint":        true,
	"complexity":  true,
	"dependency":  true,
}

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
	mux.HandleFunc("/v1/ingestor/scan-run", handleScanRun)

	log.Printf("clean-code-metric-ingestor listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

type processRequest struct {
	CommitSHA string `json:"commit_sha"`
	RepoID    string `json:"repo_id"`
	Recipe    string `json:"recipe"`
}

// runRecipe executes the scan recipe for a commit. Panicking recipes are
// recovered by the caller — this models the real ingestor's behaviour where
// a bad recipe causes a Go panic that the service catches.
func runRecipe(commitSHA, recipe string) {
	if strings.Contains(recipe, "__panic_test__") {
		panic(fmt.Sprintf("recipe %q panicked on commit %s", recipe, commitSHA))
	}
}

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

	ctx := r.Context()

	// Transition: pending -> scanning (committed to DB before any work begins
	// so that concurrent observers can witness the intermediate state).
	if _, err := db.ExecContext(ctx, `UPDATE clean_code.commit SET scan_status = 'scanning'::clean_code.scan_status, updated_at = now() WHERE sha = $1`, req.CommitSHA); err != nil {
		http.Error(w, "updating to scanning: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Brief yield so that concurrent DB pollers can observe the "scanning"
	// state before it transitions to scanned/failed.
	time.Sleep(100 * time.Millisecond)

	// Execute the recipe with panic recovery — models real ingestor behaviour
	// where a bad recipe panics and the service catches it via recover().
	var recipePanicked bool
	var panicValue interface{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				recipePanicked = true
				panicValue = r
			}
		}()
		runRecipe(req.CommitSHA, req.Recipe)
	}()

	if recipePanicked {
		log.Printf("recipe panicked: %v", panicValue)
		// Atomically: record the failed scan_run AND transition the commit to
		// 'failed'. Without the transaction, a partial write would leave an
		// orphan scan_run row with the commit stuck in 'scanning' — and the
		// E2E poller would time out with a misleading error.
		if err := finalizeScanRun(ctx, req.CommitSHA, "failed", "failed"); err != nil {
			log.Printf("finalizing failed scan_run: %v", err)
		}
		http.Error(w, fmt.Sprintf("recipe panicked: %v", panicValue), http.StatusInternalServerError)
		return
	}

	// Happy path: atomically record the succeeded scan_run AND transition
	// the commit to 'scanned'. See finalizeScanRun for the atomicity rationale.
	if err := finalizeScanRun(ctx, req.CommitSHA, "succeeded", "scanned"); err != nil {
		http.Error(w, "finalizing scan_run: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{"status":"ok"}`)
}

// finalizeScanRun inserts the terminal scan_run row and transitions the
// commit's scan_status in a single transaction so the two writes either
// both commit or neither does. If we wrote them with two autocommitted
// statements, a failure of the second one would leave an orphan scan_run
// row + the commit stuck in 'scanning' forever — observable to the E2E
// poller as a timeout with a misleading error.
//
// scanRunStatus must be a valid clean_code.scan_run_status enum value
// ('succeeded' | 'failed'). commitStatus must be a valid
// clean_code.scan_status enum value ('scanned' | 'failed').
func finalizeScanRun(ctx context.Context, commitSHA, scanRunStatus, commitStatus string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Safe even after a successful Commit: returns sql.ErrTxDone which we ignore.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.scan_run (commit_sha, kind, status, finished_at) VALUES ($1, 'ast_metrics'::clean_code.scan_run_kind, $2::clean_code.scan_run_status, now())`,
		commitSHA, scanRunStatus); err != nil {
		return fmt.Errorf("insert scan_run: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE clean_code.commit SET scan_status = $2::clean_code.scan_status, updated_at = now() WHERE sha = $1`,
		commitSHA, commitStatus); err != nil {
		return fmt.Errorf("update commit to %s: %w", commitStatus, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

type scanRunRequest struct {
	CommitSHA string `json:"commit_sha"`
	RepoID    string `json:"repo_id"`
	Kind      string `json:"kind"`
}

func handleScanRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req scanRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Application-level enum guard: reject invalid kind BEFORE reaching PostgreSQL.
	if !validScanRunKinds[req.Kind] {
		http.Error(w, fmt.Sprintf("invalid scan_run kind %q: must be one of ast_metrics, lint, complexity, dependency", req.Kind), http.StatusBadRequest)
		return
	}

	if _, err := db.Exec(`INSERT INTO clean_code.scan_run (commit_sha, kind, status) VALUES ($1, $2::clean_code.scan_run_kind, 'running'::clean_code.scan_run_status)`,
		req.CommitSHA, req.Kind); err != nil {
		http.Error(w, "inserting scan_run: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintln(w, `{"status":"created"}`)
}
