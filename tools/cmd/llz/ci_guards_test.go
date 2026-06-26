package main

import (
	"strings"
	"testing"
)

func TestRunCIRequireSecret(t *testing.T) {
	cases := []struct {
		name, secret, value, hint string
		wantExit                  int
		wantOut, wantErr          []string
	}{
		{
			name: "present", secret: "LOKI_S3_ACCESS_KEY", value: "abc",
			wantExit: 0,
			wantOut:  []string{"LOKI_S3_ACCESS_KEY: present.\n"},
		},
		{
			name: "missing without hint", secret: "OPENBAO_ROOT_TOKEN", value: "",
			wantExit: 1,
			wantErr:  []string{"::error::OPENBAO_ROOT_TOKEN is not set.\n"},
		},
		{
			name: "missing with hint", secret: "LINODE_DNS_TOKEN", value: "", hint: "Create a token with DNS scope",
			wantExit: 1,
			wantErr: []string{
				"::error::LINODE_DNS_TOKEN is not set.\n",
				"::error::Create a token with DNS scope\n",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut strings.Builder
			if got := runCIRequireSecret(tc.secret, tc.value, tc.hint, &out, &errOut); got != tc.wantExit {
				t.Fatalf("exit = %d, want %d", got, tc.wantExit)
			}
			if want := strings.Join(tc.wantOut, ""); out.String() != want {
				t.Errorf("stdout = %q, want %q", out.String(), want)
			}
			if want := strings.Join(tc.wantErr, ""); errOut.String() != want {
				t.Errorf("stderr = %q, want %q", errOut.String(), want)
			}
		})
	}
}

func TestRunCIAssertDestroyConfirm(t *testing.T) {
	cases := []struct {
		name, region, module, confirm string
		wantExit                      int
		wantErr                       string
	}{
		{name: "exact match", region: "primary", module: "cluster", confirm: "destroy:primary:cluster", wantExit: 0},
		{name: "empty", region: "primary", module: "cluster", confirm: "",
			wantExit: 1, wantErr: "::error::Set confirm_destroy to 'destroy:primary:cluster' to proceed.\n"},
		{name: "wrong module", region: "primary", module: "object-storage", confirm: "destroy:primary:cluster",
			wantExit: 1, wantErr: "::error::Set confirm_destroy to 'destroy:primary:object-storage' to proceed.\n"},
		{name: "wrong region", region: "secondary", module: "object-storage", confirm: "destroy:primary:object-storage",
			wantExit: 1, wantErr: "::error::Set confirm_destroy to 'destroy:secondary:object-storage' to proceed.\n"},
		{name: "case sensitive", region: "primary", module: "cluster", confirm: "DESTROY:primary:cluster",
			wantExit: 1, wantErr: "::error::Set confirm_destroy to 'destroy:primary:cluster' to proceed.\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errOut strings.Builder
			if got := runCIAssertDestroyConfirm(tc.region, tc.module, tc.confirm, &errOut); got != tc.wantExit {
				t.Fatalf("exit = %d, want %d", got, tc.wantExit)
			}
			if errOut.String() != tc.wantErr {
				t.Errorf("stderr = %q, want %q", errOut.String(), tc.wantErr)
			}
		})
	}
}

// The cobra wiring: flag registration, arg arity, env lookup.
func TestCIGuardCommandWiring(t *testing.T) {
	rs := ciRequireSecretCmd()
	if rs.Flags().Lookup("hint") == nil {
		t.Error("require-secret: --hint flag not registered")
	}
	if err := rs.Args(rs, []string{}); err == nil {
		t.Error("require-secret: zero args accepted, want ExactArgs(1) failure")
	}
	if err := rs.Args(rs, []string{"NAME"}); err != nil {
		t.Errorf("require-secret: one arg rejected: %v", err)
	}

	adc := ciAssertDestroyConfirmCmd()
	if err := adc.Args(adc, []string{"r", "m"}); err == nil {
		t.Error("assert-destroy-confirm: two args accepted, want ExactArgs(3) failure")
	}
	if err := adc.Args(adc, []string{"r", "m", "c"}); err != nil {
		t.Errorf("assert-destroy-confirm: three args rejected: %v", err)
	}
}
