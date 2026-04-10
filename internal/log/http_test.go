package log

import (
	"net/http"
	"testing"
)

func TestParseHTTPAccessLogMode(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		if got := parseHTTPAccessLogMode(""); got != httpAccessLogMutations {
			t.Fatalf("parseHTTPAccessLogMode() = %v, want %v", got, httpAccessLogMutations)
		}
	})

	t.Run("all", func(t *testing.T) {
		if got := parseHTTPAccessLogMode("all"); got != httpAccessLogAll {
			t.Fatalf("parseHTTPAccessLogMode(all) = %v, want %v", got, httpAccessLogAll)
		}
	})

	t.Run("off", func(t *testing.T) {
		if got := parseHTTPAccessLogMode("off"); got != httpAccessLogOff {
			t.Fatalf("parseHTTPAccessLogMode(off) = %v, want %v", got, httpAccessLogOff)
		}
	})
}

func TestShouldLogHTTPAccess(t *testing.T) {
	tests := []struct {
		name   string
		mode   httpAccessLogMode
		method string
		status int
		want   bool
	}{
		{name: "default suppresses successful get", mode: httpAccessLogMutations, method: http.MethodGet, status: http.StatusOK, want: false},
		{name: "default keeps successful put", mode: httpAccessLogMutations, method: http.MethodPut, status: http.StatusNoContent, want: true},
		{name: "default keeps failed get", mode: httpAccessLogMutations, method: http.MethodGet, status: http.StatusBadRequest, want: true},
		{name: "all keeps successful get", mode: httpAccessLogAll, method: http.MethodGet, status: http.StatusOK, want: true},
		{name: "off suppresses write too", mode: httpAccessLogOff, method: http.MethodPost, status: http.StatusCreated, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldLogHTTPAccess(tc.mode, tc.method, tc.status); got != tc.want {
				t.Fatalf("shouldLogHTTPAccess(%v, %q, %d) = %v, want %v", tc.mode, tc.method, tc.status, got, tc.want)
			}
		})
	}
}
