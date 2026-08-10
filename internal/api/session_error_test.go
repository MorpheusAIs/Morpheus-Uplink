package api

import "testing"

func TestIsSessionDeadError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"router status 400: session expired", true},
		{"router status 404: session not found", true},
		{"missing session", true},
		{"router status 500: provider failed", false},
		{"insufficient MOR balance", false},
		{"invalid venice_parameters", false},
	}
	for _, tc := range cases {
		got := isSessionDeadError(errString(tc.msg))
		if got != tc.want {
			t.Fatalf("%q: got %v want %v", tc.msg, got, tc.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
