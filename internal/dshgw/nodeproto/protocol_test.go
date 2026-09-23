package nodeproto

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenMatches(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name      string
		expected  string
		presented string
		want      bool
	}{
		{"equal", token, token, true},
		{"different", token, token + "x", false},
		{"shorter", token, token[:10], false},
		{"empty presented", token, "", false},
		{"empty expected", "", "", false},
		{"empty expected with presented", "", token, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TokenMatches(tc.expected, tc.presented); got != tc.want {
				t.Fatalf("TokenMatches = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"  Bearer   abc  ", "abc", true},
		{"BEARER abc", "abc", true},
		{"Basic abc", "", false},
		{"Bearer", "", false},
		{"Bearer   ", "", false},
		{"", "", false},
		{"abc", "", false},
	}
	for _, tc := range cases {
		got, ok := BearerToken(tc.header)
		if got != tc.want || ok != tc.ok {
			t.Errorf("BearerToken(%q) = (%q, %t), want (%q, %t)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestErrorCodes(t *testing.T) {
	err := Errorf(CodeWorkerNotRunning, "tenant %s has no worker", "alice")
	if !IsCode(err, CodeWorkerNotRunning) {
		t.Fatalf("IsCode failed for %v", err)
	}
	if IsCode(err, CodeAuthFailed) {
		t.Fatal("IsCode reported an unrelated code")
	}
	if CodeOf(err) != CodeWorkerNotRunning {
		t.Fatalf("CodeOf = %q", CodeOf(err))
	}
	wrapped := errors.New("outer: " + err.Error())
	if IsCode(wrapped, CodeWorkerNotRunning) {
		t.Fatal("a plain error must not be mistaken for a protocol error")
	}
	if !strings.Contains(err.Error(), "alice") {
		t.Fatalf("message lost its arguments: %v", err)
	}
	if CodeOf(errors.New("x")) != "" {
		t.Fatal("CodeOf must be empty for a non-protocol error")
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteValue(recorder, http.StatusOK, Health{Name: "node-a", Protocol: Version})
	resp := recorder.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	env, err := DecodeEnvelope(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !env.OK || env.Error != nil {
		t.Fatalf("envelope = %+v", env)
	}
	var health Health
	if err := json.Unmarshal(env.Value, &health); err != nil {
		t.Fatal(err)
	}
	if health.Name != "node-a" || health.Protocol != Version {
		t.Fatalf("health = %+v", health)
	}
}

func TestWriteError(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteError(recorder, http.StatusUnauthorized, CodeAuthFailed, "no token")
	resp := recorder.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	env, err := DecodeEnvelope(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != CodeAuthFailed || env.Error.Message != "no token" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestDecodeEnvelopeRejectsGarbage(t *testing.T) {
	if _, err := DecodeEnvelope(strings.NewReader("not json")); err == nil {
		t.Fatal("expected an error")
	}
}
