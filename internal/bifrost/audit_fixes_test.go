package bifrost

import (
	"encoding/json"
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
		fmt.Fprint(w, `{}`)
	})

	key := Key{ID: "key/with/slashes", Name: "key", Value: SecretRef{Ref: "env.OPENCODE_GO_API_KEY", Type: "env"}}
	if err := c.SetWeight(key, 1); err != nil {
		t.Fatalf("SetWeight: %v", err)
	}
}

func TestSetWeightPreservesCompleteBifrostSnapshot(t *testing.T) {
	const snapshot = `{
		"id":"k1",
		"name":"key-1",
		"value":{"value":"sk-********","ref":"env.OPENCODE_GO_API_KEY","type":"env"},
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

	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		fmt.Fprint(w, `{}`)
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
	if !ok || value["ref"] != "env.OPENCODE_GO_API_KEY" || value["value"] != nil {
		t.Errorf("value = %#v, want ref without masked preview", got["value"])
	}
}
