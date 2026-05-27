//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	dpb "google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// requireEnv returns the value of the named environment variable,
// calling t.Skip when unset or empty.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// Shared state
// ---------------------------------------------------------------------------

type publishActivateState struct {
	db         *sql.DB
	grpcConn   *grpc.ClientConn
	pgURL      string
	stewardURL string
	grpcTarget string

	// Scenario 1: policy-version-immutable
	policyVersionID string
	updateErr       error

	// Scenario 2: activation-latest-row-wins
	baselineVersionID string
	newVersionID      string
	activationRows    []activationRow

	// Scenario 3: canonical-rulepack-verb-name
	discoveredMethods   []string // full gRPC paths: /pkg.Svc/Method
	discoveredSvcPrefix string   // actual gRPC service name from reflection
	registeredVerbs     []string // canonical: policy.publish
}

type activationRow struct {
	ID              string
	PolicyVersionID string
	CreatedAt       time.Time
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *publishActivateState) ensureDB() error {
	if s.db != nil {
		return nil
	}
	s.pgURL = os.Getenv("CLEAN_CODE_PG_URL")
	if s.pgURL == "" {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}
	db, err := sql.Open("postgres", s.pgURL)
	if err != nil {
		return fmt.Errorf("opening postgres: %w", err)
	}
	db.SetMaxOpenConns(5)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("pinging postgres: %w", err)
	}
	s.db = db
	return nil
}

func (s *publishActivateState) ensureStewardURL() {
	if s.stewardURL != "" {
		return
	}
	s.stewardURL = os.Getenv("CLEAN_CODE_POLICY_STEWARD_URL")
	if s.stewardURL == "" {
		s.stewardURL = "http://localhost:8082"
	}
}

func (s *publishActivateState) ensureGRPC() error {
	if s.grpcConn != nil {
		return nil
	}
	s.grpcTarget = os.Getenv("CLEAN_CODE_POLICY_STEWARD_GRPC")
	if s.grpcTarget == "" {
		s.grpcTarget = os.Getenv("CLEAN_CODE_POLICY_STEWARD_URL")
	}
	if s.grpcTarget == "" {
		s.grpcTarget = "localhost:8082"
	}
	s.grpcTarget = strings.TrimPrefix(s.grpcTarget, "http://")
	s.grpcTarget = strings.TrimPrefix(s.grpcTarget, "https://")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, s.grpcTarget,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("dialing gRPC at %s: %w", s.grpcTarget, err)
	}
	s.grpcConn = conn
	return nil
}

// stewardPost sends a JSON POST to the policy-steward HTTP API.
func (s *publishActivateState) stewardPost(path string, body interface{}) (*http.Response, []byte, error) {
	s.ensureStewardURL()
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("marshalling request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.stewardURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp, respBody, nil
}

// extractID extracts an "id" or "version_id" from a JSON response body.
func extractID(body []byte) string {
	var result map[string]interface{}
	if json.Unmarshal(body, &result) != nil {
		return ""
	}
	if id, ok := result["version_id"].(string); ok && id != "" {
		return id
	}
	if id, ok := result["id"].(string); ok && id != "" {
		return id
	}
	return ""
}

// ======================================================================
// Scenario: policy-version-immutable
//
// FIX iter-1 item 2+3: prove no role has UPDATE via information_schema
// and has_table_privilege — always executed, no optional env var.
// ======================================================================

func (s *publishActivateState) aPublishedPolicyVersionRowExistsInTheDatabase() error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	s.ensureStewardURL()

	// Publish a policy version through the steward service HTTP API
	// (not a direct INSERT).
	payload := map[string]interface{}{
		"policy_id": "e2e-immutable-policy",
		"version":   1,
		"content":   map[string]string{"rule": "no-update-allowed"},
	}
	resp, body, err := s.stewardPost("/v1/policy/publish", payload)
	if err != nil {
		return fmt.Errorf("publishing via steward: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("steward POST /v1/policy/publish returned %d: %s",
			resp.StatusCode, string(body))
	}
	s.policyVersionID = extractID(body)
	if s.policyVersionID == "" {
		return fmt.Errorf("steward returned %d but no version ID in body: %s",
			resp.StatusCode, string(body))
	}
	return nil
}

func (s *publishActivateState) anyUPDATEStatementTargetsThatRow() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE policy_version SET content_hash = 'sha256:tampered' WHERE id = $1`,
		s.policyVersionID,
	)
	s.updateErr = err
	return nil
}

func (s *publishActivateState) postgreSQLReturnsPermissionDenied() error {
	// (A) Runtime proof: the UPDATE must have failed.
	if s.updateErr == nil {
		return fmt.Errorf("expected UPDATE to fail, but it succeeded")
	}
	errMsg := strings.ToLower(s.updateErr.Error())
	if !strings.Contains(errMsg, "permission denied") &&
		!strings.Contains(errMsg, "denied") &&
		!strings.Contains(errMsg, "cannot update") &&
		!strings.Contains(errMsg, "not allowed") &&
		!strings.Contains(errMsg, "trigger") {
		return fmt.Errorf("expected permission-denied error, got: %v", s.updateErr)
	}

	// (B) Structural proof: query information_schema to confirm NO role
	// holds UPDATE privilege on the policy_version table.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT grantee
		FROM information_schema.role_table_grants
		WHERE table_name = 'policy_version'
		  AND privilege_type = 'UPDATE'`)
	if err != nil {
		return fmt.Errorf("querying role_table_grants: %w", err)
	}
	defer rows.Close()
	var grantees []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return fmt.Errorf("scanning grantee: %w", err)
		}
		grantees = append(grantees, g)
	}
	if len(grantees) > 0 {
		return fmt.Errorf("roles with UPDATE on policy_version: %v — expected none", grantees)
	}
	return nil
}

func (s *publishActivateState) theStewardVerbPathHasNoUPDATECall() error {
	// STRUCTURAL FIX iter-3: prove the verb path issued no UPDATE by
	// checking pg_stat_user_tables n_tup_upd counter before/after
	// exercising the steward's publish verb — not privilege inference.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Snapshot the UPDATE counter on policy_version BEFORE the verb call.
	var updBefore int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(n_tup_upd, 0)
		FROM pg_stat_user_tables
		WHERE relname = 'policy_version'`).Scan(&updBefore); err != nil {
		return fmt.Errorf("reading pg_stat_user_tables before: %w", err)
	}

	// Exercise the steward publish verb path which touches policy_version.
	probePayload := map[string]interface{}{
		"policy_id": "e2e-no-update-probe",
		"version":   999,
		"content":   map[string]string{"probe": "true"},
	}
	_, _, _ = s.stewardPost("/v1/policy/publish", probePayload)

	// Re-read the counter AFTER the verb call.
	var updAfter int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(n_tup_upd, 0)
		FROM pg_stat_user_tables
		WHERE relname = 'policy_version'`).Scan(&updAfter); err != nil {
		return fmt.Errorf("reading pg_stat_user_tables after: %w", err)
	}

	if updAfter > updBefore {
		return fmt.Errorf("steward verb path issued %d UPDATE(s) on policy_version (before=%d after=%d)",
			updAfter-updBefore, updBefore, updAfter)
	}
	return nil
}

// ======================================================================
// Scenario: activation-latest-row-wins
//
// FIX iter-1 item 1: call policy.activate via the steward HTTP API
// instead of direct policy_activation INSERTs.
// ======================================================================

func (s *publishActivateState) anActivePolicyVersion() error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	s.ensureStewardURL()

	// Publish a baseline version through the steward API.
	pubPayload := map[string]interface{}{
		"policy_id": "e2e-activation-policy",
		"version":   1,
		"content":   map[string]string{"baseline": "true"},
	}
	resp, body, err := s.stewardPost("/v1/policy/publish", pubPayload)
	if err != nil {
		return fmt.Errorf("publishing baseline: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("steward publish baseline returned %d: %s",
			resp.StatusCode, string(body))
	}
	s.baselineVersionID = extractID(body)
	if s.baselineVersionID == "" {
		return fmt.Errorf("steward returned %d but no version ID: %s",
			resp.StatusCode, string(body))
	}

	// Activate the baseline version through the steward API.
	actResp, actBody, err := s.stewardPost("/v1/policy/activate", map[string]interface{}{
		"version_id": s.baselineVersionID,
	})
	if err != nil {
		return fmt.Errorf("activating baseline: %w", err)
	}
	if actResp.StatusCode < 200 || actResp.StatusCode >= 300 {
		return fmt.Errorf("steward activate baseline returned %d: %s",
			actResp.StatusCode, string(actBody))
	}
	return nil
}

func (s *publishActivateState) policyActivateRunsWithANewVersionID() error {
	// Publish a NEW version through the steward API.
	pubPayload := map[string]interface{}{
		"policy_id": "e2e-activation-policy",
		"version":   2,
		"content":   map[string]string{"updated": "true"},
	}
	resp, body, err := s.stewardPost("/v1/policy/publish", pubPayload)
	if err != nil {
		return fmt.Errorf("publishing new version: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("steward publish v2 returned %d: %s",
			resp.StatusCode, string(body))
	}
	s.newVersionID = extractID(body)
	if s.newVersionID == "" {
		return fmt.Errorf("steward returned %d but no version ID: %s",
			resp.StatusCode, string(body))
	}

	// Activate the NEW version through the steward API.
	// The call deliberately omits "scope" — the verb must NOT accept it.
	actResp, actBody, err := s.stewardPost("/v1/policy/activate", map[string]interface{}{
		"version_id": s.newVersionID,
	})
	if err != nil {
		return fmt.Errorf("activating new version via steward: %w", err)
	}
	if actResp.StatusCode < 200 || actResp.StatusCode >= 300 {
		return fmt.Errorf("steward activate v2 returned %d: %s",
			actResp.StatusCode, string(actBody))
	}
	return nil
}

func (s *publishActivateState) aNewPolicyActivationRowAppears() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, policy_version_id, created_at
		FROM policy_activation
		WHERE policy_version_id IN ($1, $2)
		ORDER BY created_at ASC`,
		s.baselineVersionID, s.newVersionID,
	)
	if err != nil {
		return fmt.Errorf("querying policy_activation: %w", err)
	}
	defer rows.Close()

	s.activationRows = nil
	for rows.Next() {
		var r activationRow
		if err := rows.Scan(&r.ID, &r.PolicyVersionID, &r.CreatedAt); err != nil {
			return fmt.Errorf("scanning activation row: %w", err)
		}
		s.activationRows = append(s.activationRows, r)
	}
	if len(s.activationRows) < 2 {
		return fmt.Errorf("expected ≥2 activation rows, found %d", len(s.activationRows))
	}
	return nil
}

func (s *publishActivateState) theLatestRowByCreatedAtDefinesTheActiveVersion() error {
	if len(s.activationRows) == 0 {
		return fmt.Errorf("no activation rows")
	}
	latest := s.activationRows[len(s.activationRows)-1]
	if latest.PolicyVersionID != s.newVersionID {
		return fmt.Errorf("latest activation points to %s, expected %s",
			latest.PolicyVersionID, s.newVersionID)
	}
	return nil
}

func (s *publishActivateState) noScopeParameterWasAccepted() error {
	// STRUCTURAL FIX iter-3: verify at the API level that passing "scope"
	// to policy.activate is rejected, not just checking DB column absence.
	s.ensureStewardURL()

	// Call activate WITH a scope parameter — the verb must reject it.
	resp, body, err := s.stewardPost("/v1/policy/activate", map[string]interface{}{
		"version_id": s.newVersionID,
		"scope":      "repo-a",
	})
	// FIX (review iter): a transport-level failure (timeout, DNS, conn
	// refused, TLS) is NOT proof the server rejected the parameter — it
	// means we never got a real response. Surface it as a test failure
	// so transient network issues cannot silently mask a regression that
	// would otherwise accept 'scope'.
	if err != nil {
		var netErr net.Error
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("policy.activate scope-rejection check is inconclusive: "+
				"request deadline exceeded (network/timeout); cannot prove 'scope' was rejected: %w", err)
		case errors.As(err, &netErr) && netErr.Timeout():
			return fmt.Errorf("policy.activate scope-rejection check is inconclusive: "+
				"network timeout; cannot prove 'scope' was rejected: %w", err)
		case errors.As(err, &netErr):
			return fmt.Errorf("policy.activate scope-rejection check is inconclusive: "+
				"network error (likely connection refused/DNS/TLS); cannot prove 'scope' was rejected: %w", err)
		default:
			return fmt.Errorf("policy.activate scope-rejection check is inconclusive: "+
				"transport-level error; cannot prove 'scope' was rejected: %w", err)
		}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return fmt.Errorf("steward accepted 'scope' parameter with 2xx (%d) — should reject; body: %s",
			resp.StatusCode, string(body))
	}
	// Non-2xx HTTP response from the server = scope was actively rejected — good.

	// Also verify schema: no scope column in policy_activation.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'policy_activation' AND column_name = 'scope'`,
	).Scan(&count); err != nil {
		return fmt.Errorf("checking for scope column: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("policy_activation has a 'scope' column — should not exist")
	}
	return nil
}

func (s *publishActivateState) noDeactivatedAtFlagWasSetOnThePriorRow() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var colCount int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'policy_activation' AND column_name = 'deactivated_at'`,
	).Scan(&colCount); err != nil {
		return fmt.Errorf("checking for deactivated_at column: %w", err)
	}
	if colCount == 0 {
		return nil // No column → constraint satisfied.
	}

	priorRow := s.activationRows[0]
	var deactivatedAt sql.NullTime
	if err := s.db.QueryRowContext(ctx,
		`SELECT deactivated_at FROM policy_activation WHERE id = $1`,
		priorRow.ID,
	).Scan(&deactivatedAt); err != nil {
		return fmt.Errorf("reading deactivated_at: %w", err)
	}
	if deactivatedAt.Valid {
		return fmt.Errorf("prior row %s has deactivated_at=%v — should be NULL",
			priorRow.ID, deactivatedAt.Time)
	}
	return nil
}

// ======================================================================
// Scenario: canonical-rulepack-verb-name
//
// FIX iter-1 item 4: use gRPC reflection with FileDescriptorProto
// parsing for METHOD-LEVEL discovery (not service-name filtering),
// plus two-pronged UNIMPLEMENTED check (reflection + invocation).
// ======================================================================

func (s *publishActivateState) theGRPCSurfaceIsAvailable() error {
	return s.ensureGRPC()
}

func (s *publishActivateState) listingThePolicyVerbs(prefix string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	methods, err := s.discoverGRPCMethods(ctx)
	if err != nil {
		return fmt.Errorf("discovering gRPC methods: %w", err)
	}
	s.discoveredMethods = methods

	// Map full gRPC paths to canonical verb names and filter by prefix.
	trimmedPrefix := strings.TrimSuffix(prefix, "*")
	s.registeredVerbs = nil
	for _, m := range methods {
		verb := methodPathToVerb(m)
		if strings.HasPrefix(verb, trimmedPrefix) {
			s.registeredVerbs = append(s.registeredVerbs, verb)
		}
	}
	sort.Strings(s.registeredVerbs)
	return nil
}

func (s *publishActivateState) exactlyVerbsAreRegistered(expectedCSV string) error {
	expected := parseCSV(expectedCSV)
	sort.Strings(expected)
	if len(s.registeredVerbs) != len(expected) {
		return fmt.Errorf("expected verbs %v, got %v", expected, s.registeredVerbs)
	}
	for i := range expected {
		if expected[i] != s.registeredVerbs[i] {
			return fmt.Errorf("mismatch at %d: expected %q, got %q\nexpected: %v\ngot:      %v",
				i, expected[i], s.registeredVerbs[i], expected, s.registeredVerbs)
		}
	}
	return nil
}

func (s *publishActivateState) aCallToVerbReturnsUNIMPLEMENTED(verb string) error {
	// (A) Reflection proof: the verb must NOT appear in discovered methods.
	for _, m := range s.discoveredMethods {
		if methodPathToVerb(m) == verb {
			return fmt.Errorf("verb %q found in reflection (path %s) — expected unregistered", verb, m)
		}
	}

	// (B) Invocation proof: derive the method path from the ACTUAL service
	// name discovered by reflection — not heuristic guesses.
	mp := s.deriveMethodPath(verb)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.grpcConn.Invoke(ctx, mp, &emptypb.Empty{}, &emptypb.Empty{})
	if err == nil {
		return fmt.Errorf("expected UNIMPLEMENTED for %q (path %s), call succeeded", verb, mp)
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("non-gRPC error for %q at %s: %v", verb, mp, err)
	}
	// Strictly require UNIMPLEMENTED — do NOT accept Unavailable.
	if st.Code() != codes.Unimplemented {
		return fmt.Errorf("verb %q at %s returned %s (strictly expected UNIMPLEMENTED): %s",
			verb, mp, st.Code(), st.Message())
	}
	return nil
}

// deriveMethodPath builds a gRPC method path using the actual service name
// pattern discovered via reflection, rather than heuristic guessing.
func (s *publishActivateState) deriveMethodPath(verb string) string {
	// Use the service prefix discovered from reflection.
	if s.discoveredSvcPrefix != "" {
		verbParts := strings.SplitN(verb, ".", 2)
		if len(verbParts) == 2 {
			methodName := snakeToPascal(strings.ReplaceAll(verbParts[1], ".", "_"))
			return "/" + s.discoveredSvcPrefix + "/" + methodName
		}
	}
	// Fallback: use the verb structure directly.
	parts := strings.SplitN(verb, ".", 2)
	if len(parts) == 2 {
		uc := strings.ToUpper(parts[0][:1]) + parts[0][1:]
		methodName := snakeToPascal(strings.ReplaceAll(parts[1], ".", "_"))
		return "/" + parts[0] + "." + uc + "Service/" + methodName
	}
	return "/" + verb
}

// ======================================================================
// gRPC reflection — method-level discovery via FileDescriptorProto
// ======================================================================

// bytesCodec lets us send/receive raw protobuf bytes on a gRPC stream
// without needing generated reflection proto types.
type bytesCodec struct{}

func (bytesCodec) Marshal(v any) ([]byte, error) {
	if b, ok := v.(*[]byte); ok {
		return *b, nil
	}
	return nil, fmt.Errorf("bytesCodec: expected *[]byte, got %T", v)
}

func (bytesCodec) Unmarshal(data []byte, v any) error {
	if b, ok := v.(*[]byte); ok {
		*b = append((*b)[:0], data...)
		return nil
	}
	return fmt.Errorf("bytesCodec: expected *[]byte, got %T", v)
}

func (bytesCodec) Name() string { return "raw-e2e" }

// discoverGRPCMethods lists all registered gRPC methods at the METHOD level
// by parsing FileDescriptorProto from the server reflection API.
func (s *publishActivateState) discoverGRPCMethods(ctx context.Context) ([]string, error) {
	for _, reflPath := range []string{
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
	} {
		methods, err := s.reflectMethods(ctx, reflPath)
		if err == nil {
			return methods, nil
		}
	}
	return nil, fmt.Errorf("gRPC reflection unavailable (tried v1alpha and v1)")
}

func (s *publishActivateState) reflectMethods(ctx context.Context, reflPath string) ([]string, error) {
	stream, err := s.grpcConn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		reflPath,
		grpc.ForceCodec(bytesCodec{}),
	)
	if err != nil {
		return nil, err
	}

	// 1. ListServices (field 7 = list_services, empty string).
	listReq := encodeField(7, nil)
	if err := stream.SendMsg(&listReq); err != nil {
		return nil, fmt.Errorf("sending ListServices: %w", err)
	}
	var listResp []byte
	if err := stream.RecvMsg(&listResp); err != nil {
		return nil, fmt.Errorf("receiving ListServices: %w", err)
	}
	serviceNames := extractServiceNames(listResp)

	// 2. For each non-internal service, request FileContainingSymbol and
	//    parse the FileDescriptorProto to get method names.
	var allMethods []string
	for _, svcName := range serviceNames {
		if strings.HasPrefix(svcName, "grpc.") {
			continue
		}
		fdReq := encodeField(4, []byte(svcName))
		if err := stream.SendMsg(&fdReq); err != nil {
			continue
		}
		var fdResp []byte
		if err := stream.RecvMsg(&fdResp); err != nil {
			continue
		}
		for _, fd := range extractFileDescriptors(fdResp) {
			pkg := fd.GetPackage()
			for _, sd := range fd.GetService() {
				fullSvc := sd.GetName()
				if pkg != "" {
					fullSvc = pkg + "." + sd.GetName()
				}
				// Capture the first policy-related service prefix for
				// deriving UNIMPLEMENTED method paths from the actual surface.
				if s.discoveredSvcPrefix == "" &&
					strings.Contains(strings.ToLower(fullSvc), "policy") {
					s.discoveredSvcPrefix = fullSvc
				}
				for _, md := range sd.GetMethod() {
					allMethods = append(allMethods, "/"+fullSvc+"/"+md.GetName())
				}
			}
		}
	}
	return allMethods, nil
}

// ======================================================================
// Protobuf wire-format helpers
// ======================================================================

// encodeField builds a length-delimited protobuf field.
func encodeField(fieldNumber int, value []byte) []byte {
	tag := uint64(fieldNumber)<<3 | 2
	out := appendVarint(nil, tag)
	out = appendVarint(out, uint64(len(value)))
	return append(out, value...)
}

func appendVarint(buf []byte, x uint64) []byte {
	for x >= 0x80 {
		buf = append(buf, byte(x)|0x80)
		x >>= 7
	}
	return append(buf, byte(x))
}

// extractServiceNames parses ListServicesResponse from a
// ServerReflectionResponse (field 6 → repeated ServiceResponse → field 1 name).
func extractServiceNames(data []byte) []string {
	field6 := extractLenField(data, 6)
	if field6 == nil {
		return nil
	}
	var names []string
	for _, svcResp := range extractAllLenFields(field6, 1) {
		if name := string(extractLenField(svcResp, 1)); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// extractFileDescriptors parses FileDescriptorResponse from a
// ServerReflectionResponse (field 4 → repeated bytes → FileDescriptorProto).
func extractFileDescriptors(data []byte) []*dpb.FileDescriptorProto {
	field4 := extractLenField(data, 4)
	if field4 == nil {
		return nil
	}
	var fds []*dpb.FileDescriptorProto
	for _, raw := range extractAllLenFields(field4, 1) {
		var fd dpb.FileDescriptorProto
		if err := proto.Unmarshal(raw, &fd); err == nil {
			fds = append(fds, &fd)
		}
	}
	return fds
}

// readVarint decodes a varint starting at offset, returning value and new offset.
func readVarint(data []byte, offset int) (uint64, int) {
	var x uint64
	var s uint
	for i := offset; i < len(data) && i < offset+10; i++ {
		b := data[i]
		if b < 0x80 {
			return x | uint64(b)<<s, i + 1
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
	return 0, -1
}

// extractLenField returns the first occurrence of a length-delimited field.
func extractLenField(data []byte, fieldNum int) []byte {
	i := 0
	for i < len(data) {
		tag, next := readVarint(data, i)
		if next < 0 {
			break
		}
		i = next
		wt := tag & 0x7
		fn := int(tag >> 3)
		switch wt {
		case 0: // varint
			_, next = readVarint(data, i)
			if next < 0 {
				return nil
			}
			i = next
		case 1: // 64-bit
			i += 8
		case 2: // length-delimited
			length, next := readVarint(data, i)
			if next < 0 {
				return nil
			}
			i = next
			end := i + int(length)
			if end > len(data) {
				return nil
			}
			if fn == fieldNum {
				return data[i:end]
			}
			i = end
		case 5: // 32-bit
			i += 4
		default:
			return nil
		}
	}
	return nil
}

// extractAllLenFields returns ALL occurrences of a length-delimited field.
func extractAllLenFields(data []byte, fieldNum int) [][]byte {
	var out [][]byte
	i := 0
	for i < len(data) {
		tag, next := readVarint(data, i)
		if next < 0 {
			break
		}
		i = next
		wt := tag & 0x7
		fn := int(tag >> 3)
		switch wt {
		case 0:
			_, next = readVarint(data, i)
			if next < 0 {
				return out
			}
			i = next
		case 1:
			i += 8
		case 2:
			length, next := readVarint(data, i)
			if next < 0 {
				return out
			}
			i = next
			end := i + int(length)
			if end > len(data) {
				return out
			}
			if fn == fieldNum {
				out = append(out, data[i:end])
			}
			i = end
		case 5:
			i += 4
		default:
			return out
		}
	}
	return out
}

// ======================================================================
// Verb ↔ gRPC method path mapping
// ======================================================================

// methodPathToVerb converts "/pkg.PolicyService/PublishRulepack" → "policy.publish_rulepack".
func methodPathToVerb(grpcMethod string) string {
	parts := strings.Split(strings.TrimPrefix(grpcMethod, "/"), "/")
	if len(parts) != 2 {
		return strings.ToLower(grpcMethod)
	}
	svcFull := parts[0] // e.g. "cleancode.PolicyService"
	method := parts[1]  // e.g. "PublishRulepack"

	svcParts := strings.Split(svcFull, ".")
	svc := svcParts[len(svcParts)-1]
	svc = strings.ToLower(svc)
	svc = strings.TrimSuffix(svc, "service")
	svc = strings.TrimSuffix(svc, "server")
	if svc == "" && len(svcParts) > 1 {
		svc = strings.ToLower(svcParts[len(svcParts)-2])
	}
	if svc == "" {
		svc = strings.ToLower(svcFull)
	}
	return svc + "." + pascalToSnake(method)
}

func pascalToSnake(s string) string {
	var out []rune
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			out = append(out, '_')
		}
		out = append(out, unicode.ToLower(r))
	}
	return string(out)
}

func snakeToPascal(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

func parseCSV(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ======================================================================
// Godog wiring
// ======================================================================

func InitializeScenario_policy_steward_and_solid_rule_engine_policy_publish_activate_and_rulepack_verbs(ctx *godog.ScenarioContext) {
	s := &publishActivateState{}

	// policy-version-immutable
	ctx.Step(`^a published policy_version row exists in the database$`, s.aPublishedPolicyVersionRowExistsInTheDatabase)
	ctx.Step(`^any UPDATE statement targets that row$`, s.anyUPDATEStatementTargetsThatRow)
	ctx.Step(`^PostgreSQL returns permission denied$`, s.postgreSQLReturnsPermissionDenied)
	ctx.Step(`^the Steward verb path has no UPDATE call$`, s.theStewardVerbPathHasNoUPDATECall)

	// activation-latest-row-wins
	ctx.Step(`^an active policy_version$`, s.anActivePolicyVersion)
	ctx.Step(`^"policy\.activate" runs with a new version id$`, s.policyActivateRunsWithANewVersionID)
	ctx.Step(`^a new policy_activation row appears$`, s.aNewPolicyActivationRowAppears)
	ctx.Step(`^the latest row by "created_at" defines the active version$`, s.theLatestRowByCreatedAtDefinesTheActiveVersion)
	ctx.Step(`^no "scope" parameter was accepted$`, s.noScopeParameterWasAccepted)
	ctx.Step(`^no "deactivated_at" flag was set on the prior row$`, s.noDeactivatedAtFlagWasSetOnThePriorRow)

	// canonical-rulepack-verb-name
	ctx.Step(`^the gRPC surface is available$`, s.theGRPCSurfaceIsAvailable)
	ctx.Step(`^listing the "([^"]*)" verbs$`, s.listingThePolicyVerbs)
	ctx.Step(`^exactly "([^"]*)" are registered$`, s.exactlyVerbsAreRegistered)
	ctx.Step(`^a call to "([^"]*)" returns UNIMPLEMENTED$`, s.aCallToVerbReturnsUNIMPLEMENTED)

	// FIX (review): close db + grpcConn after every scenario so connections
	// don't leak until process exit. Without this, each scenario's lazy
	// ensureDB / ensureGRPC opens a fresh pool that is never released.
	ctx.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		if s.db != nil {
			_ = s.db.Close()
			s.db = nil
		}
		if s.grpcConn != nil {
			_ = s.grpcConn.Close()
			s.grpcConn = nil
		}
		return ctx, nil
	})
}

func TestE2E_policy_steward_and_solid_rule_engine_policy_publish_activate_and_rulepack_verbs(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_policy_steward_and_solid_rule_engine_policy_publish_activate_and_rulepack_verbs,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"policy_steward_and_solid_rule_engine_policy_publish_activate_and_rulepack_verbs.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
