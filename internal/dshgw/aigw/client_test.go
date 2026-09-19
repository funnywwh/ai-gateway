package aigw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateKeySemantics(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    []string
		invalid bool
	}{{"invalid", 401, `{"error":"x"}`, nil, true}, {"empty is valid", 200, `{"data":[]}`, []string{}, false}, {"models", 200, `{"data":[{"id":"a"},{"id":"a"},{"id":"b"}]}`, []string{"a", "b"}, false}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer sk-abcdefghijkl" {
					t.Errorf("authorization=%q", r.Header.Get("Authorization"))
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			got, err := (&Client{BaseURL: srv.URL, HTTP: srv.Client()}).ValidateKey(context.Background(), "sk-abcdefghijkl")
			if tt.invalid {
				if !errors.Is(err, ErrInvalidKey) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got=%v", got)
			}
		})
	}
}
func TestValidateKeyOtherStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	defer srv.Close()
	_, err := (&Client{BaseURL: srv.URL}).ValidateKey(context.Background(), "sk-abcdefghijkl")
	var status *StatusError
	if !errors.As(err, &status) || status.Status != 503 {
		t.Fatalf("err=%v", err)
	}
}
func TestKeyPrefix(t *testing.T) {
	p, err := KeyPrefix("123456789012rest")
	if err != nil || p != "123456789012" {
		t.Fatalf("%q %v", p, err)
	}
	if _, err := KeyPrefix("short"); err == nil {
		t.Fatal("short accepted")
	}
}

func TestValidateKeyRejectsRedirectMissingDataAndOversize(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{{"redirect", 302, ""}, {"missing-data", 200, `{}`}, {"oversize", 200, strings.Repeat("x", (1<<20)+1)}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 302 {
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if _, err := (&Client{BaseURL: srv.URL}).ValidateKey(context.Background(), "sk-abcdefghijkl"); err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
}

func TestNormalizeKey(t *testing.T) {
	got, err := NormalizeKey("  Bearer sk-abcdefghijkl  ")
	if err != nil || got != "sk-abcdefghijkl" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	for _, bad := range []string{"sk-abc defghijkl", "密钥abcdefghijkl"} {
		if _, err := NormalizeKey(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// serveModels answers one /v1/models body and hands back the parsed models.
func serveModels(t *testing.T, body string) []Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	models, err := (&Client{BaseURL: srv.URL, HTTP: srv.Client()}).ValidateKey(context.Background(), "sk-abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	return models
}

// TestValidateKeyDisclosesModelFacts is the M68 contract on this side: the listing's
// capability facts survive the trip, and the three meanings of a missing reasoning claim
// (supports it / declared without it / said nothing) stay distinguishable — DSH renders a
// different setting for each.
func TestValidateKeyDisclosesModelFacts(t *testing.T) {
	models := serveModels(t, `{"object":"list","data":[
		{"id":"deepseek-flash","name":" DeepSeek Flash ","context_window":1000000,"max_output_tokens":65536,
		 "input_modalities":["text","image"],"capabilities":{"stream":true,"tools":true,"reasoning":true}},
		{"id":"u2-flash","input_modalities":["text"],"capabilities":{"stream":true,"tools":true}},
		{"id":"capability-only-image","capabilities":{"stream":true,"image":true}},
		{"id":"forced","capabilities":{"reasoning":true},"reasoning":{"mode":"force","effort":"high"}}
	]}`)
	byID := map[string]Model{}
	for _, model := range models {
		byID[model.ID] = model
	}
	if len(models) != 4 {
		t.Fatalf("models = %+v", models)
	}

	flash := byID["deepseek-flash"]
	if flash.Name != "DeepSeek Flash" || flash.ContextWindow != 1000000 || flash.MaxOutputTokens != 65536 {
		t.Errorf("flash facts = %+v", flash)
	}
	if !flash.Images {
		t.Error("flash must accept images: input_modalities listed image")
	}
	if flash.ReasoningSupported == nil || !*flash.ReasoningSupported || flash.ReasoningForced {
		t.Errorf("flash reasoning = %v forced=%t, want supported and not forced", flash.ReasoningSupported, flash.ReasoningForced)
	}

	plain := byID["u2-flash"]
	if plain.Name != "" || plain.ContextWindow != 0 || plain.MaxOutputTokens != 0 || plain.Images {
		t.Errorf("plain facts = %+v, want nothing disclosed", plain)
	}
	if plain.ReasoningSupported == nil || *plain.ReasoningSupported {
		t.Errorf("u2-flash reasoning = %v, want a disclosed false (declared without reasoning)", plain.ReasoningSupported)
	}

	// The capability key alone is enough: either spelling of the same fact is read, so a
	// consumer never has to know which one an endpoint filled in.
	if !byID["capability-only-image"].Images {
		t.Error("a model declaring capabilities.image must accept images even without input_modalities")
	}

	forced := byID["forced"]
	if forced.ReasoningSupported == nil || !*forced.ReasoningSupported || !forced.ReasoningForced {
		t.Errorf("forced reasoning = %v forced=%t, want supported and forced", forced.ReasoningSupported, forced.ReasoningForced)
	}
}

// An older aigw answers ids only, and a row with a malformed fact loses that fact alone:
// neither may fail the key validation that gates a tenant's whole model list.
func TestValidateKeyToleratesMissingAndMalformedFacts(t *testing.T) {
	models := serveModels(t, `{"object":"list","data":[
		{"id":"legacy"},
		{"id":"broken","name":7,"context_window":"1M","max_output_tokens":-5,
		 "input_modalities":"text","capabilities":"nope","reasoning":42}
	]}`)
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	for _, model := range models {
		if model.Name != "" || model.ContextWindow != 0 || model.MaxOutputTokens != 0 || model.Images {
			t.Errorf("%s = %+v, want nothing disclosed", model.ID, model)
		}
		if model.ReasoningSupported != nil || model.ReasoningForced {
			t.Errorf("%s reasoning = %v forced=%t, want an undisclosed claim", model.ID, model.ReasoningSupported, model.ReasoningForced)
		}
	}
}
