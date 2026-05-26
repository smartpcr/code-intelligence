package webhook_test

import (
	"strings"
	"testing"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/webhook"
)

// TestValidateVerbToken_ClosedSet pins the registration-time
// guard the Router uses for `/v1/ingest/{verb}` segments.
// The closed set is documented on [webhook.ValidateVerbToken]:
// non-empty, length <= 64, every byte in `[a-z_]`.
func TestValidateVerbToken_ClosedSet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "happy_churn", token: "churn", wantErr: false},
		{name: "happy_test_balance", token: "test_balance", wantErr: false},
		{name: "happy_underscore", token: "external_per_row", wantErr: false},
		{name: "empty", token: "", wantErr: true},
		{name: "uppercase", token: "Churn", wantErr: true},
		{name: "with_digit", token: "churn2", wantErr: true},
		{name: "with_hyphen", token: "test-balance", wantErr: true},
		{name: "with_dot", token: "ingest.churn", wantErr: true},
		{name: "with_slash", token: "v1/churn", wantErr: true},
		{name: "with_space", token: "test balance", wantErr: true},
		{name: "too_long", token: strings.Repeat("a", 65), wantErr: true},
		{name: "max_length_ok", token: strings.Repeat("a", 64), wantErr: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := webhook.ValidateVerbToken(tc.token)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateVerbToken(%q): want error, got nil", tc.token)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateVerbToken(%q): want nil, got %v", tc.token, err)
			}
		})
	}
}
