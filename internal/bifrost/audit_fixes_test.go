package bifrost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestKeysRejectsOversizedResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", maxResponseBody+1))
	})

	_, err := c.Keys()
	if err == nil || !strings.Contains(err.Error(), "trop volumineuse") {
		t.Fatalf("Keys error = %v, want oversized-response error", err)
	}
}

func TestSetWeightEscapesKeyID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.EscapedPath(), "/api/providers/opencode-go/keys/key%2Fwith%2Fslashes"; got != want {
			t.Errorf("escaped path = %q, want %q", got, want)
		}
		fmt.Fprint(w, `{"id":"key/with/slashes","weight":1}`)
	})

	key := Key{ID: "key/with/slashes", Name: "key", Status: "success", Value: SecretRef{Ref: "env.OPENCODE_GO_API_KEY", Type: "env"}}
	if err := c.SetWeight(key, 1); err != nil {
		t.Fatalf("SetWeight: %v", err)
	}
}

func TestSetWeightPreservesCompleteBifrostSnapshot(t *testing.T) {
	const snapshot = `{
		"id":"k1",
		"name":"key-1",
		"value":{"value":"sk-********","ref":"env.OPENCODE_GO_API_KEY","type":"env","scope":"team"},
		"models":["model-a"],
		"blacklisted_models":["model-b"],
		"weight":1,
		"enabled":false,
		"use_for_batch_api":true,
		"use_anthropic_endpoints":true,
		"aliases":{"public-model":"provider-model"},
		"description":"managed manually",
		"future_option":{"enabled":true},
		"status":"success"
	}`
	var key Key
	if err := json.Unmarshal([]byte(snapshot), &key); err != nil {
		t.Fatalf("Unmarshal snapshot: %v", err)
	}
	if key.Enabled == nil || *key.Enabled {
		t.Fatalf("Enabled = %v, want explicit false preserved as a policy input", key.Enabled)
	}

	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		fmt.Fprint(w, `{"id":"k1","weight":0.5}`)
	})
	if err := c.SetWeight(key, 0.5); err != nil {
		t.Fatalf("SetWeight: %v", err)
	}

	for _, field := range []string{
		"blacklisted_models", "enabled", "use_for_batch_api",
		"use_anthropic_endpoints", "aliases", "description", "future_option", "status",
	} {
		if _, ok := got[field]; !ok {
			t.Errorf("payload lost field %q: %#v", field, got)
		}
	}
	if got["enabled"] != false || got["weight"] != 0.5 {
		t.Errorf("enabled/weight = %v/%v, want false/0.5", got["enabled"], got["weight"])
	}
	value, ok := got["value"].(map[string]any)
	if !ok || value["ref"] != "env.OPENCODE_GO_API_KEY" || value["value"] != nil || value["scope"] != "team" {
		t.Errorf("value = %#v, want ref and future fields without masked preview", got["value"])
	}
}

func TestClientsRefuseRedirects(t *testing.T) {
	redirected := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			redirected++
			fmt.Fprint(w, keysPayload)
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusFound)
	})

	if _, err := c.Keys(); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("Keys redirect error = %v, want HTTP 302", err)
	}
	if redirected != 0 {
		t.Fatalf("redirect target called %d time(s), want 0", redirected)
	}

	key := Key{
		ID: "k1", Name: "k1", Status: "success",
		Value: SecretRef{Ref: "env.OPENCODE_GO_API_KEY", Type: "env"},
	}
	if err := c.SetWeight(key, 1); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("SetWeight redirect error = %v, want HTTP 302", err)
	}
	if redirected != 0 {
		t.Fatalf("redirect target called %d time(s), want 0", redirected)
	}
}

func TestSetWeightRefusesUnmanagedSecretReferences(t *testing.T) {
	requests := 0
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { requests++ })

	for name, value := range map[string]SecretRef{
		"plaintext":      {Type: "plain_text", Value: "secret"},
		"other env":      {Type: "env", Ref: "env.OTHER_SECRET"},
		"empty suffix":   {Type: "env", Ref: "env.OPENCODE_GO_API_KEY_"},
		"missing prefix": {Type: "env", Ref: "OPENCODE_GO_API_KEY"},
	} {
		t.Run(name, func(t *testing.T) {
			key := Key{ID: "k1", Name: "k1", Status: "success", Value: value}
			if err := c.SetWeight(key, 1); err == nil || !strings.Contains(err.Error(), "non gérée") {
				t.Fatalf("SetWeight error = %v, want unmanaged-reference refusal", err)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests)
	}
}

func TestKeyContextFetchesAndValidatesSingleKey(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.EscapedPath(), "/api/providers/opencode-go/keys/key%2Fone"; got != want {
			t.Errorf("escaped path = %q, want %q", got, want)
		}
		fmt.Fprint(w, `{"id":"key/one","name":"one","value":{"ref":"env.OPENCODE_GO_API_KEY","type":"env"},"models":[],"weight":0.5,"status":"success"}`)
	})

	key, err := c.KeyContext(context.Background(), "key/one")
	if err != nil {
		t.Fatalf("KeyContext: %v", err)
	}
	if key.ID != "key/one" || key.Weight != 0.5 {
		t.Fatalf("key = %+v", key)
	}
}

func TestBifrostContextCancellation(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, keysPayload)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.KeysContext(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("KeysContext error = %v, want context.Canceled", err)
	}
}

func TestSetWeightRequiresMatchingConfirmation(t *testing.T) {
	for name, response := range map[string]string{
		"missing fields": `{}`,
		"wrong id":       `{"id":"other","weight":1}`,
		"wrong weight":   `{"id":"k1","weight":0}`,
		"invalid json":   `{`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, response)
			})
			key := Key{
				ID: "k1", Name: "k1", Status: "success",
				Value: SecretRef{Ref: "env.OPENCODE_GO_API_KEY", Type: "env"},
			}
			if err := c.SetWeight(key, 1); err == nil || !strings.Contains(err.Error(), "confirmation") {
				t.Fatalf("SetWeight error = %v, want invalid confirmation", err)
			}
		})
	}
}
