package bifrost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const keysPayload = `{
  "keys": [
    {
      "id": "cface157-ee47-4ee6-87e7-d9491ffa71f7",
      "name": "opencode-go-key-1",
      "value": {"value": "sk-V************************iqyd", "ref": "env.OPENCODE_GO_API_KEY", "type": "env"},
      "models": ["*"], "weight": 0, "status": "success"
    },
    {
      "id": "opencode-go-key-3",
      "name": "opencode-go-key-3",
      "value": {"value": "sk-s************************iLeC", "ref": "env.OPENCODE_GO_API_KEY_A", "type": "env"},
      "models": ["*"], "weight": 1, "status": "success"
    }
  ],
  "total": 2
}`

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, 2*time.Second)
}

func TestKeys(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/providers/opencode-go/keys" {
			t.Errorf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, keysPayload)
	})

	keys, err := c.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %d, want 2", len(keys))
	}
	if keys[0].Weight != 0 || keys[0].Status != "success" {
		t.Errorf("key-1 = %+v", keys[0])
	}
	if keys[1].Value.Ref != "env.OPENCODE_GO_API_KEY_A" {
		t.Errorf("key-3 ref = %q", keys[1].Value.Ref)
	}
}

// SetWeight must re-state the env reference (never the masked secret preview)
// and the model list, because the update endpoint replaces fields instead of
// merging them.
func TestSetWeightSendsFullPayload(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/api/providers/opencode-go/keys/opencode-go-key-3" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"opencode-go-key-3","weight":0}`)
	})

	key := Key{
		ID:     "opencode-go-key-3",
		Name:   "opencode-go-key-3",
		Value:  SecretRef{Ref: "env.OPENCODE_GO_API_KEY_A", Type: "env"},
		Models: []string{"*"},
		Weight: 1,
		Status: "success",
	}
	if err := c.SetWeight(key, 0); err != nil {
		t.Fatalf("SetWeight: %v", err)
	}

	if got["weight"] != float64(0) {
		t.Errorf("weight = %v, want 0", got["weight"])
	}
	if got["id"] != "opencode-go-key-3" || got["name"] != "opencode-go-key-3" {
		t.Errorf("id/name = %v/%v", got["id"], got["name"])
	}
	value, ok := got["value"].(map[string]any)
	if !ok {
		t.Fatalf("value = %v", got["value"])
	}
	if value["ref"] != "env.OPENCODE_GO_API_KEY_A" || value["type"] != "env" {
		t.Errorf("value = %v, want ref+type only", value)
	}
	if raw := value["value"]; raw != nil {
		t.Errorf("value.value = %v — masked secret must never be sent back", raw)
	}
	models, ok := got["models"].([]any)
	if !ok || len(models) != 1 || models[0] != "*" {
		t.Errorf("models = %v, want [\"*\"]", got["models"])
	}
}

// An empty model whitelist means "allow none" in Bifrost and must remain
// empty rather than being broadened to ["*"].
func TestSetWeightPreservesEmptyModels(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"k1","weight":1}`)
	})

	key := Key{
		ID: "k1", Name: "k1", Status: "success",
		Value:  SecretRef{Ref: "env.OPENCODE_GO_API_KEY", Type: "env"},
		Models: []string{},
	}
	if err := c.SetWeight(key, 1); err != nil {
		t.Fatalf("SetWeight: %v", err)
	}
	models, ok := got["models"].([]any)
	if !ok || len(models) != 0 {
		t.Errorf("models = %v, want an empty list", got["models"])
	}
}

func TestKeysSurfacesErrors(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := c.Keys(); err == nil {
		t.Fatal("expected an error on HTTP 500")
	}

	c2 := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"Key value must not be empty"}`)
	})
	err := c2.SetWeight(Key{
		ID: "k1", Name: "k1", Status: "success",
		Value: SecretRef{Ref: "env.OPENCODE_GO_API_KEY", Type: "env"},
	}, 1)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("SetWeight error = %v, want HTTP 400", err)
	}
	if strings.Contains(err.Error(), "Key value must not be empty") {
		t.Errorf("SetWeight leaked remote response body: %v", err)
	}
}
