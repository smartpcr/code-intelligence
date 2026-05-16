package repoindexer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"sync"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

// defaultWorkerID synthesises a per-process worker identifier
// for the `ingest_jobs.claimed_by` column. The shape is
// `repoindexer-<host>-<pid>-<6 bytes random hex>`; the host/pid
// prefix lets operators correlate a queue row's `claimed_by`
// with the producing pod in cluster logs, and the random suffix
// de-duplicates per worker goroutine within the same process.
func defaultWorkerID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	pid := itoa(os.Getpid())
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// rand.Read in modern Go does not fail; if it ever
		// does the worker ID falls back to the host+pid label
		// rather than panicking the whole service.
		return "repoindexer-" + host + "-" + pid + "-fallback"
	}
	return "repoindexer-" + host + "-" + pid + "-" + hex.EncodeToString(buf[:])
}

// Pool runs N independent Workers concurrently. It is the
// implementation-plan §3.1 "Add a worker-pool config so 4
// workers run in parallel against the §8.3 200 k LOC ≤ 30 min
// target" surface.
//
// The pool itself is intentionally trivial -- the load-bearing
// claim-exclusivity primitive is the per-worker SELECT FOR
// UPDATE SKIP LOCKED transaction; the pool just spawns the
// goroutines. Each worker carries its own WorkerID so the queue
// rows show distinct `claimed_by` values.
type Pool struct {
	workers []*Worker
}

// PoolConfig captures the operator-tunable knobs for a pool.
//
// REQUIRED fields (NewPool ultimately delegates to NewWorker,
// which PANICS on nil for these — see worker.go WorkerOptions
// docstring):
//   - PerWorker.Materializer
//   - PerWorker.Publisher
//
// OPTIONAL fields (NewPool / NewWorker apply defaults):
//   - Workers (defaults to DefaultWorkers = 4)
//   - PerWorker.WorkerID (the pool synthesises a unique ID per
//     spawned goroutine, with the "#<i>" suffix appended even
//     when the caller supplied a base value, so claim rows are
//     always distinguishable)
//   - PerWorker.PollInterval, PerWorker.Clock, PerWorker.Logger,
//     PerWorker.MaxAttempts, PerWorker.ASTEmitter (see
//     WorkerOptions for per-field defaults)
type PoolConfig struct {
	// Workers is the number of concurrent polling loops. The
	// §8.3 target of "200 k LOC ≤ 30 min" with one worker per
	// CPU-bound parser invocation maps to 4 on the typical
	// 4-vCPU pod the deployment targets. Operators with
	// different shapes (more vCPUs, IO-heavier upstream)
	// adjust this directly. Optional; defaults to
	// DefaultWorkers when zero.
	Workers int
	// PerWorker is the WorkerOptions template each spawned
	// worker is instantiated with. Its Materializer and
	// Publisher fields are REQUIRED — every spawned worker
	// inherits the same pointers, and NewWorker panics if
	// either is nil. The pool overrides PerWorker.WorkerID per
	// worker so each loop carries a distinct claim identity
	// even when the caller leaves the field blank.
	PerWorker WorkerOptions
}

// DefaultWorkers is the operator default for `Workers`. The
// number is pinned to the §8.3 acceptance target ("4 workers
// against the 200 k LOC ≤ 30 min" budget per
// implementation-plan.md). Operators with different deployment
// shapes override via PoolConfig.Workers.
const DefaultWorkers = 4

// NewPool spawns `cfg.Workers` workers over `db`/`writer`. Each
// spawned Worker is constructed via NewWorker, so the
// "Materializer / Publisher REQUIRED" contract documented on
// WorkerOptions fires here: NewPool panics through NewWorker if
// either pointer is nil on `cfg.PerWorker`. This keeps the
// "must pass a Materializer and Publisher" contract honest at
// construction time rather than at the first claim.
//
// Returns an error when `cfg.Workers < 1` -- a pool with zero
// workers is a misconfiguration that would silently make the
// queue back up forever.
func NewPool(db *sql.DB, writer *graphwriter.Writer, cfg PoolConfig) (*Pool, error) {
	n := cfg.Workers
	if n == 0 {
		n = DefaultWorkers
	}
	if n < 1 {
		return nil, errors.New("repoindexer: NewPool: workers must be >= 1")
	}
	workers := make([]*Worker, 0, n)
	for i := 0; i < n; i++ {
		opts := cfg.PerWorker
		// Per-worker WorkerID so claim rows distinguish each
		// goroutine even when the caller didn't pre-populate
		// the field. If the caller supplied a base WorkerID
		// we append "#i" to keep the prefix and still
		// disambiguate.
		base := opts.WorkerID
		if base == "" {
			base = defaultWorkerID()
		}
		opts.WorkerID = base + "#" + itoa(i)
		workers = append(workers, NewWorker(db, writer, opts))
	}
	return &Pool{workers: workers}, nil
}

// Workers exposes the spawned workers so tests can inspect
// individual identities or drive `ProcessOnce` against a
// specific worker. Production callers should use `Run`.
func (p *Pool) Workers() []*Worker {
	out := make([]*Worker, len(p.workers))
	copy(out, p.workers)
	return out
}

// Run starts all workers concurrently and returns once every
// worker's Run returns. The pool's Run propagates the FIRST
// non-context-cancellation error any worker surfaces back to
// the caller, while still waiting for the other workers to
// finish so the caller does not leak goroutines.
//
// `ctx` cancellation signals every worker to stop; the pool
// returns ctx.Err() in that case.
func (p *Pool) Run(ctx context.Context) error {
	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		firstE  error
	)
	for _, w := range p.workers {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				errOnce.Do(func() { firstE = err })
			}
		}()
	}
	wg.Wait()
	if firstE != nil {
		return firstE
	}
	return ctx.Err()
}

// itoa is a tiny int-to-decimal helper local to this file so
// the pool's WorkerID synthesis avoids pulling strconv into the
// import set. The values it formats are tiny (worker indices)
// so the 32-bit ceiling is irrelevant.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
