package probe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type checkTransport func(*http.Request) (*http.Response, error)

func (f checkTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestAccessCheckNeverExchangesRefreshToken(t *testing.T) {
	for _, tc := range []struct {
		code         int
		body, status string
	}{
		{200, `{"plan_type":"team"}`, "ok"},
		{401, `{"error":"invalid_token"}`, "needs_refresh"},
		{403, `<html>challenge</html>`, "unknown"},
		{403, `{"error":{"code":"account_deactivated"}}`, "banned"},
		{401, `{"error":{"code":"account_banned"}}`, "banned"},
		{401, `{"error":{"code":"workspace_deactivated"}}`, "workspace_deactivated"},
		{403, `{"detail":{"code":"deactivated_workspace"}}`, "workspace_deactivated"},
		{401, `{"error":{"message":"account_banned workspace_deactivated"}}`, "needs_refresh"},
		{403, `{"error":"workspace_deactivated"}`, "workspace_deactivated"},
		{429, `{}`, "unknown"}, {503, `{}`, "unknown"}, {200, `<html>login</html>`, "unknown"},
	} {
		t.Run(tc.status+tc.body, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: checkTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != "GET" || req.URL.String() != openAIUsageURL || req.Body != nil || req.Header.Get("Authorization") != "Bearer fixture-at" || req.Header.Get("Chatgpt-Account-Id") != "fixture-workspace" {
					t.Fatal("test changed into a credential exchange")
				}
				return &http.Response{StatusCode: tc.code, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
			})}
			creds := map[string]any{"access_token": "fixture-at", "refresh_token": "fixture-rt", "id_token": "fixture-id", "account_id": "fixture-workspace"}
			result := checkAccessToken(client, creds)
			if result.Status != tc.status || calls != 1 || creds["refresh_token"] != "fixture-rt" || creds["access_token"] != "fixture-at" || creds["id_token"] != "fixture-id" {
				t.Fatalf("unexpected check result: %+v calls=%d", result, calls)
			}
		})
	}
	if result := OAuth(map[string]any{"refresh_token": "fixture-rt"}, "", 0); result.Status != "needs_refresh" {
		t.Fatal(result)
	}
}

func TestAccessCheckRetriesTransportFailureButNever401OrRefresh(t *testing.T) {
	for _, tc := range []struct {
		failureCode, failures, wantCalls int
		want                             string
	}{
		{0, 2, 3, "ok"}, {0, 4, 3, "unknown"}, {503, 1, 2, "ok"}, {401, 4, 1, "needs_refresh"}, {429, 4, 1, "unknown"},
	} {
		calls := 0
		client := &http.Client{Transport: checkTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Method != "GET" || req.Body != nil {
				t.Fatal("non-read-only retry")
			}
			code := 200
			if calls <= tc.failures {
				if tc.failureCode == 0 {
					return nil, errors.New("fixture TLS handshake failed")
				}
				code = tc.failureCode
			}
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(`{"plan_type":"team"}`)), Header: http.Header{}}, nil
		})}
		result := checkAccessTokenWithRetry(context.Background(), client, map[string]any{"access_token": "fixture-at", "refresh_token": "fixture-rt"}, []time.Duration{0, 0})
		if calls != tc.wantCalls || result.Status != tc.want {
			t.Fatal(tc, calls, result)
		}
	}
}
