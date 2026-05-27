package management

// Stage 6.2 -- RepoStore: the persistence seam used by the
// `mgmt.register_repo` and `mgmt.set_mode` HTTP verbs.
//
// # Architectural invariant (architecture Sec 6.3)
//
// "Mgmt surface never writes Measurement rows directly -- it
//  only emits `repo_event` and delegates."
//
// The two onboarding verbs DO write the catalog-side `repo`
// row (architecture Sec 5.1.1) directly -- that is the
// Management role's primary write surface per the C5 ACL in
// `0004_roles.up.sql:311`. The accompanying `repo_event`
// append (kind=`registered` / `mode_changed`) is the
// audit-log record of the mutation.
//
// # Atomicity contract
//
// Both `RegisterRepo` and `SetRepoMode` are atomic from the
// caller's perspective: either BOTH the catalog mutation
// AND the matching `repo_event` row land, or NEITHER does.
// The in-memory implementation here holds a single mutex
// around both writes; the PG-backed implementation (a
// follow-up stage) will use one transaction.
//
// This eliminates the half-applied state that the rubber-
// duck review flagged for an early "lookup-then-insert"
// design: if the event append fails after the row insert,
// the state-change is observable without an audit trail
// (or vice versa). The atomic seam makes the failure
// case "all-or-nothing".
//
// # Idempotency contract
//
// `RegisterRepo` is idempotent ON `repo_url`. The first
// call mints a fresh `repo_id` AND appends one
// `repo_event(kind='registered')`. Every subsequent call
// with the same URL returns the existing `repo_id` with
// `Created=false` AND DOES NOT append a second
// `registered` event -- the architecture's append-only
// audit log carries one `registered` row per repo
// lifecycle, not one per re-registration attempt.
//
// `SetRepoMode` is idempotent ON `(repo_id, new_mode)`. A
// call with the mode already in effect returns
// `Changed=false` AND DOES NOT append a `mode_changed`
// event. This matches architecture Sec 5.1.4: a
// `mode_changed` row records a TRANSITION, not a
// re-assertion.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gofrs/uuid"
)

// Canonical mode values for `clean_code.repo.mode` per
// architecture Sec 5.1.1 line 878 (`embedded` default per
// operator pin `ast-mode-default`, Sec 1.6) and the
// `clean_code.repo_mode` enum in `0001_catalog_lifecycle.up.sql`.
const (
	// RepoModeEmbedded is the default mode -- the Clean
	// Code service runs standalone with tree-sitter AST and
	// no agent-memory dependency.
	RepoModeEmbedded = "embedded"
	// RepoModeLinked is the cross-repo composition mode --
	// the AST Adapter reuses `agent-memory.GraphReader`.
	RepoModeLinked = "linked"
)

// Canonical RepoEvent kind literals for the Stage 6.2
// verbs. Past-tense forms per architecture Sec 5.1.4 line
// 910 (`registered`, `retired`, `retract_intent`,
// `mode_changed`).
//
// Pinned as string constants (not an enum) so a grep for
// the literal `"registered"` / `"mode_changed"` reveals
// every reference (DB enum label, architecture text,
// tech-spec invariant, handler call sites).
const (
	RepoEventKindRegistered  = "registered"
	RepoEventKindModeChanged = "mode_changed"
)

// AllowedRepoModes is the closed list of legal values for
// `clean_code.repo.mode`. Pinned as a Go slice so the
// validation helper can iterate once and emit a clear
// "got X, want one of [...]" error.
var AllowedRepoModes = []string{RepoModeEmbedded, RepoModeLinked}

// IsAllowedRepoMode reports whether `mode` is one of the
// two canonical values. Whitespace is NOT trimmed -- the
// caller is expected to trim before calling so the error
// message can echo the raw input back to the operator.
func IsAllowedRepoMode(mode string) bool {
	for _, allowed := range AllowedRepoModes {
		if mode == allowed {
			return true
		}
	}
	return false
}

// Sentinel errors surfaced by [RepoStore] implementations.
var (
	// ErrRepoStoreUnknownRepo signals that the supplied
	// repo_id has no matching `clean_code.repo` row. The
	// HTTP layer maps this to 404.
	ErrRepoStoreUnknownRepo = errors.New("management: RepoStore: repo_id not found")

	// ErrRepoStoreInvalidMode signals that the supplied
	// mode is outside the canonical [AllowedRepoModes]
	// set. The HTTP layer maps this to 400 (the wire
	// validator catches it first; this sentinel is a
	// belt-and-braces guard for direct Go callers).
	ErrRepoStoreInvalidMode = errors.New("management: RepoStore: invalid mode (allowed: embedded, linked)")

	// ErrRepoStoreEmptyURL signals that the supplied repo
	// URL is empty after [strings.TrimSpace]. The HTTP
	// layer maps this to 400.
	ErrRepoStoreEmptyURL = errors.New("management: RepoStore: repo_url is empty")

	// ErrRepoStoreEmptyDefaultBranch signals that the
	// supplied default_branch is empty after trim.
	ErrRepoStoreEmptyDefaultBranch = errors.New("management: RepoStore: default_branch is empty")

	// ErrRepoStoreZeroRepoID signals that the supplied
	// repo_id is the zero UUID. The HTTP layer maps this
	// to 400 (a separate "zero-uuid" check fires before
	// store dispatch).
	ErrRepoStoreZeroRepoID = errors.New("management: RepoStore: repo_id is the zero UUID")
)

// RegisterRepoRowRequest is the request shape consumed by
// [RepoStore.RegisterRepo]. Mirrors the wire surface of
// `mgmt.register_repo(repo_url, default_branch, modes)`
// per architecture Sec 6.3 + impl-plan line 603.
//
// Field-level invariants:
//
//   - `RepoURL` MUST be non-empty after [strings.TrimSpace].
//     This value is the WRITE-ONCE column on
//     `clean_code.repo.repo_url` (migration
//     `0006_repo_url.up.sql:97-115`).
//   - `DefaultBranch` MUST be non-empty after trim.
//   - `Mode`, when non-empty, MUST be in [AllowedRepoModes].
//     An empty value defers to the schema default
//     (`embedded`).
//   - `DisplayName`, when empty, is derived from `RepoURL`
//     (architecture Sec 5.1.1 line 877: "free-form"). This
//     keeps the wire payload minimal (the brief's verb
//     signature names only `repo_url, default_branch,
//     modes`) while honouring the `NOT NULL` constraint on
//     `clean_code.repo.display_name`.
//   - `Actor` MUST be the trimmed `X-OIDC-Subject` header
//     value, supplied by the wire layer; the store stamps
//     it onto the `repo_event.payload_json.actor` field
//     so the audit row carries the operator who registered
//     the repo (workstream brief: "Each write verb requires
//     an authenticated caller (OIDC subject) recorded as
//     `actor` on the resulting RepoEvent ...").
type RegisterRepoRowRequest struct {
	RepoURL       string
	DefaultBranch string
	Mode          string
	DisplayName   string
	Actor         string
}

// RegisterRepoResult is the response shape of
// [RepoStore.RegisterRepo]. `Created` discriminates the
// fresh-insert from the idempotent re-register path.
type RegisterRepoResult struct {
	// RepoID is the catalog primary key. On the fresh
	// path this is a server-minted UUID; on the
	// idempotent path it is the existing row's
	// `repo_id`.
	RepoID uuid.UUID
	// Created is `true` iff a new row was inserted (and
	// a `repo_event(kind='registered')` was appended).
	// `false` iff the URL was already known and the
	// existing repo_id is returned unchanged.
	Created bool
	// Mode is the effective mode stored on the row. On
	// the fresh path this is either the request's
	// `Mode` (if supplied) or [RepoModeEmbedded] (the
	// schema default). On the idempotent path it
	// reflects the EXISTING row's mode (the request's
	// `Mode` is ignored if the row already exists --
	// callers MUST use `mgmt.set_mode` to change mode
	// on an existing repo).
	Mode string
}

// SetRepoModeRequest is the request shape consumed by
// [RepoStore.SetRepoMode]. Mirrors the wire surface of
// `mgmt.set_mode(repo_id, mode)`.
//
// Field-level invariants:
//
//   - `RepoID` MUST be non-zero.
//   - `Mode` MUST be in [AllowedRepoModes].
//   - `Actor` MUST be the trimmed `X-OIDC-Subject` header
//     value.
type SetRepoModeRequest struct {
	RepoID uuid.UUID
	Mode   string
	Actor  string
}

// SetRepoModeResult is the response shape of
// [RepoStore.SetRepoMode]. `Changed` discriminates a real
// mode transition (which appended a
// `repo_event(kind='mode_changed')` row) from a no-op
// re-assertion of the existing mode.
type SetRepoModeResult struct {
	// RepoID echoes the request input.
	RepoID uuid.UUID
	// Mode is the effective mode AFTER the call (the
	// new value if Changed=true, the unchanged
	// existing value if Changed=false).
	Mode string
	// PreviousMode is the value the row carried BEFORE
	// the call. Equal to Mode iff Changed=false.
	PreviousMode string
	// Changed is `true` iff the row was UPDATEd AND a
	// `repo_event(kind='mode_changed')` row was
	// appended. `false` iff PreviousMode == Mode and
	// neither write occurred (canonical no-op).
	Changed bool
}

// RepoStore is the atomic persistence seam for the
// `mgmt.register_repo` and `mgmt.set_mode` verbs.
//
// Both methods own BOTH the catalog mutation (insert /
// update `clean_code.repo`) AND the matching `repo_event`
// audit-row append. Production implementations MUST
// guarantee the two writes appear atomic to concurrent
// observers (via a transaction or equivalent).
//
// # Why one seam, not two
//
// An earlier design split this into
// `LookupRepoIDByURL`+`InsertRepo`+`AppendRepoEvent` calls
// on the handler side. The rubber-duck review flagged two
// blocking issues with that shape:
//
//  1. The check-then-act `LookupRepoIDByURL`+`InsertRepo`
//     sequence races on concurrent `register_repo` calls
//     for the same URL (no DB-level unique constraint on
//     `repo_url`); two rows can be inserted.
//  2. Splitting the catalog mutation from the
//     `repo_event` append leaves a half-applied state if
//     the second call fails: a mode change without an
//     audit row, or vice versa, breaking architecture
//     Sec 5.1.4's append-only contract.
//
// Folding both writes behind a single atomic seam fixes
// both: the in-memory implementation serialises via a
// mutex, and the PG implementation (future) will run
// both statements in one transaction.
type RepoStore interface {
	// RegisterRepo atomically performs the
	// `clean_code.repo` INSERT and the matching
	// `clean_code.repo_event(kind='registered',
	// payload_json={repo_url, default_branch, mode,
	// display_name, actor})` APPEND.
	//
	// On the idempotent path (URL already known) the
	// existing repo_id is returned with `Created=false`;
	// no second `registered` event is appended (the
	// audit log carries ONE `registered` per repo
	// lifecycle).
	//
	// Returns [ErrRepoStoreEmptyURL] /
	// [ErrRepoStoreEmptyDefaultBranch] /
	// [ErrRepoStoreInvalidMode] for the validation
	// failures the handler also catches; defensive depth
	// against a direct Go caller bypassing the wire.
	RegisterRepo(ctx context.Context, req RegisterRepoRowRequest) (RegisterRepoResult, error)

	// SetRepoMode atomically UPDATEs `clean_code.repo.mode`
	// and APPENDs
	// `clean_code.repo_event(kind='mode_changed',
	// payload_json={mode, previous_mode, actor})`. When
	// the new mode equals the row's current mode the
	// call is a no-op: `Changed=false`, no UPDATE issued,
	// no event appended.
	//
	// Returns [ErrRepoStoreUnknownRepo] when the
	// repo_id is not found; the handler maps that to 404.
	// Returns [ErrRepoStoreInvalidMode] /
	// [ErrRepoStoreZeroRepoID] for the validation
	// failures the handler also catches.
	SetRepoMode(ctx context.Context, req SetRepoModeRequest) (SetRepoModeResult, error)
}

// repoStoreRecord is the in-memory shape of one persisted
// `clean_code.repo` row. The store owns this slice; tests
// inspect via [InMemoryRepoStore.Rows].
type repoStoreRecord struct {
	RepoID        uuid.UUID
	RepoURL       string
	DefaultBranch string
	Mode          string
	DisplayName   string
}

// InMemoryRepoStore is the in-process [RepoStore]
// implementation used by unit tests and the early
// composition root (pre-PG bring-up). The store carries
// the persisted `repo` rows in append order; tests
// inspect them via [Rows] / [Lookup].
//
// All mutating methods hold a single mutex around BOTH
// the repo-row write AND the [RepoEventAppender] call so
// observers see either both writes or neither. This is
// the in-memory analogue of the PG transaction the
// follow-up stage will introduce.
//
// Concurrency: the mutex serialises every operation;
// parallel test cases sharing one store land their
// writes in a deterministic order.
type InMemoryRepoStore struct {
	mu       sync.Mutex
	rows     []repoStoreRecord
	byURL    map[string]uuid.UUID
	byRepoID map[uuid.UUID]int // index into rows
	// appender is the [RepoEventAppender] used to persist
	// the `registered` / `mode_changed` audit rows. The
	// store calls AppendRepoEvent INSIDE the mutex so the
	// repo-row mutation and the event append are atomic
	// from concurrent observers' perspective.
	appender RepoEventAppender
	// newRepoID is the UUID minter, defaulting to
	// [uuid.NewV4]. Tests inject a deterministic minter
	// for assertion stability.
	newRepoID func() (uuid.UUID, error)
}

// NewInMemoryRepoStore constructs an [InMemoryRepoStore].
// `appender` MUST be non-nil; the store calls it inside
// its mutex to append the matching `repo_event` row. A
// nil appender would cause a nil-pointer panic on the
// first registration -- failing fast at the constructor
// catches the wiring bug earlier.
func NewInMemoryRepoStore(appender RepoEventAppender) *InMemoryRepoStore {
	if appender == nil {
		panic("management: NewInMemoryRepoStore: appender is nil (production wiring MUST supply a RepoEventAppender)")
	}
	return &InMemoryRepoStore{
		rows:      nil,
		byURL:     make(map[string]uuid.UUID),
		byRepoID:  make(map[uuid.UUID]int),
		appender:  appender,
		newRepoID: uuid.NewV4,
	}
}

// SetRepoIDMinter overrides the default [uuid.NewV4] minter
// used to allocate `repo_id` values on fresh inserts. Tests
// inject a deterministic minter so assertions can pin
// exact UUID values (rather than chasing the mint at
// runtime). Production never calls this.
func (s *InMemoryRepoStore) SetRepoIDMinter(mint func() (uuid.UUID, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mint == nil {
		s.newRepoID = uuid.NewV4
		return
	}
	s.newRepoID = mint
}

// RegisterRepo implements [RepoStore]. See the interface
// godoc for the atomicity / idempotency contract.
func (s *InMemoryRepoStore) RegisterRepo(ctx context.Context, req RegisterRepoRowRequest) (RegisterRepoResult, error) {
	if err := ctx.Err(); err != nil {
		return RegisterRepoResult{}, err
	}
	url := strings.TrimSpace(req.RepoURL)
	if url == "" {
		return RegisterRepoResult{}, ErrRepoStoreEmptyURL
	}
	branch := strings.TrimSpace(req.DefaultBranch)
	if branch == "" {
		return RegisterRepoResult{}, ErrRepoStoreEmptyDefaultBranch
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		// DB default per `0001_catalog_lifecycle.up.sql:154`.
		mode = RepoModeEmbedded
	}
	if !IsAllowedRepoMode(mode) {
		return RegisterRepoResult{}, fmt.Errorf("%w: got %q", ErrRepoStoreInvalidMode, mode)
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		// Architecture Sec 5.1.1 line 877: "free-form".
		// Derive a friendly default from the URL's path
		// tail (e.g. `https://github.com/org/repo.git` ->
		// `repo`). Keeps the brief's verb signature
		// `(repo_url, default_branch, modes)` payload-
		// minimal while honouring the NOT NULL constraint
		// on `clean_code.repo.display_name` AND giving
		// operators a meaningful label (the full URL would
		// clutter list views).
		displayName = deriveDisplayNameFromURL(url)
	}
	actor := strings.TrimSpace(req.Actor)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Idempotent path: same URL already registered. Return
	// the existing repo_id WITHOUT appending a second
	// `registered` event -- the architecture's append-only
	// audit log holds ONE `registered` per repo lifecycle.
	if existingID, ok := s.byURL[url]; ok {
		idx := s.byRepoID[existingID]
		existing := s.rows[idx]
		return RegisterRepoResult{
			RepoID:  existingID,
			Created: false,
			Mode:    existing.Mode,
		}, nil
	}

	// Fresh path: mint a repo_id, append the row, then
	// append the `registered` event. Both writes are
	// inside the mutex so concurrent observers see
	// either both or neither.
	repoID, err := s.newRepoID()
	if err != nil {
		return RegisterRepoResult{}, fmt.Errorf("management: InMemoryRepoStore: mint repo_id: %w", err)
	}
	rec := repoStoreRecord{
		RepoID:        repoID,
		RepoURL:       url,
		DefaultBranch: branch,
		Mode:          mode,
		DisplayName:   displayName,
	}
	s.rows = append(s.rows, rec)
	s.byURL[url] = repoID
	s.byRepoID[repoID] = len(s.rows) - 1

	payload := map[string]any{
		"repo_url":       url,
		"default_branch": branch,
		"mode":           mode,
		"display_name":   displayName,
	}
	if actor != "" {
		payload["actor"] = actorPrefix + actor
	}
	if err := s.appender.AppendRepoEvent(ctx, repoID, RepoEventKindRegistered, payload); err != nil {
		// Roll back the in-memory row insert so the
		// caller can retry without leaking a
		// half-applied state. The PG transaction will
		// handle this automatically via ROLLBACK; the
		// in-memory store mirrors that contract.
		s.rows = s.rows[:len(s.rows)-1]
		delete(s.byURL, url)
		delete(s.byRepoID, repoID)
		return RegisterRepoResult{}, fmt.Errorf("management: InMemoryRepoStore: append registered event: %w", err)
	}

	return RegisterRepoResult{
		RepoID:  repoID,
		Created: true,
		Mode:    mode,
	}, nil
}

// SetRepoMode implements [RepoStore]. See the interface
// godoc for the atomicity / no-op-on-same-mode contract.
func (s *InMemoryRepoStore) SetRepoMode(ctx context.Context, req SetRepoModeRequest) (SetRepoModeResult, error) {
	if err := ctx.Err(); err != nil {
		return SetRepoModeResult{}, err
	}
	if req.RepoID == uuid.Nil {
		return SetRepoModeResult{}, ErrRepoStoreZeroRepoID
	}
	mode := strings.TrimSpace(req.Mode)
	if !IsAllowedRepoMode(mode) {
		return SetRepoModeResult{}, fmt.Errorf("%w: got %q", ErrRepoStoreInvalidMode, mode)
	}
	actor := strings.TrimSpace(req.Actor)

	s.mu.Lock()
	defer s.mu.Unlock()

	idx, ok := s.byRepoID[req.RepoID]
	if !ok {
		return SetRepoModeResult{}, fmt.Errorf("%w: repo_id=%s", ErrRepoStoreUnknownRepo, req.RepoID)
	}
	previous := s.rows[idx].Mode
	if previous == mode {
		// Canonical no-op: row already at the target
		// mode. No UPDATE issued, no event appended --
		// `mode_changed` records a TRANSITION, not a
		// re-assertion.
		return SetRepoModeResult{
			RepoID:       req.RepoID,
			Mode:         previous,
			PreviousMode: previous,
			Changed:      false,
		}, nil
	}

	// Real transition: UPDATE the row, append the event.
	// Both inside the mutex for atomicity.
	s.rows[idx].Mode = mode

	payload := map[string]any{
		"mode":          mode,
		"previous_mode": previous,
	}
	if actor != "" {
		payload["actor"] = actorPrefix + actor
	}
	if err := s.appender.AppendRepoEvent(ctx, req.RepoID, RepoEventKindModeChanged, payload); err != nil {
		// Roll back the in-memory UPDATE.
		s.rows[idx].Mode = previous
		return SetRepoModeResult{}, fmt.Errorf("management: InMemoryRepoStore: append mode_changed event: %w", err)
	}

	return SetRepoModeResult{
		RepoID:       req.RepoID,
		Mode:         mode,
		PreviousMode: previous,
		Changed:      true,
	}, nil
}

// Rows returns a snapshot of every persisted row. Test
// helper -- production code reads via the management
// `Reader` (Stage 6.3 follow-up).
func (s *InMemoryRepoStore) Rows() []repoStoreRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]repoStoreRecord, len(s.rows))
	copy(out, s.rows)
	return out
}

// Lookup returns the row for `repoID`. Test helper.
func (s *InMemoryRepoStore) Lookup(repoID uuid.UUID) (repoStoreRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.byRepoID[repoID]
	if !ok {
		return repoStoreRecord{}, false
	}
	return s.rows[idx], true
}

// LookupByURL returns the row for `url`. Test helper.
func (s *InMemoryRepoStore) LookupByURL(url string) (repoStoreRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byURL[strings.TrimSpace(url)]
	if !ok {
		return repoStoreRecord{}, false
	}
	idx := s.byRepoID[id]
	return s.rows[idx], true
}

// Count returns the number of persisted rows. Test helper.
func (s *InMemoryRepoStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

// Compile-time interface guard.
var _ RepoStore = (*InMemoryRepoStore)(nil)

// deriveDisplayNameFromURL extracts a friendly label from a
// repo URL by taking the LAST non-empty path segment and
// stripping a trailing `.git` suffix. Examples:
//
//	"https://github.com/org/repo"     -> "repo"
//	"https://github.com/org/repo.git" -> "repo"
//	"https://github.com/org/repo/"    -> "repo"
//	"git@github.com:org/repo.git"     -> "repo"
//	""                                -> ""
//
// The helper never returns an empty string for a non-empty
// input: if no path tail can be extracted (e.g. the input
// has no `/` and no `:`), the trimmed input is returned
// verbatim. The caller (the store) only invokes this on
// already-validated non-empty URLs, so the empty-input case
// is impossible at the call site.
//
// This honours architecture Sec 5.1.1 line 877
// ("display_name is free-form") AND the
// `clean_code.repo.display_name NOT NULL` constraint --
// the brief's verb signature `(repo_url, default_branch,
// modes)` omits `display_name`, so callers SHOULD be able
// to register without supplying it.
func deriveDisplayNameFromURL(url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}
	// Trim trailing slashes so a URL like
	// `https://github.com/org/repo/` derives "repo" instead
	// of "".
	url = strings.TrimRight(url, "/")
	// Take the last path segment. We split on BOTH `/` and
	// `:` so SCP-style URLs (`git@github.com:org/repo.git`)
	// reduce to "repo".
	cut := strings.LastIndexAny(url, "/:")
	tail := url
	if cut >= 0 && cut < len(url)-1 {
		tail = url[cut+1:]
	}
	// Strip a trailing `.git` (the most common URL suffix).
	tail = strings.TrimSuffix(tail, ".git")
	if tail == "" {
		// Pathological case: input was all separators.
		// Fall back to the trimmed original so the NOT NULL
		// constraint is honoured.
		return url
	}
	return tail
}
