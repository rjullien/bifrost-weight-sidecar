// Package bifrost reads provider key state from the Bifrost gateway and
// updates key weights through its management API.
package bifrost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	provider        = "opencode-go"
	maxResponseBody = 1 << 20 // 1 MiB
)

// Key is one provider key as exposed by the Bifrost management API.
type Key struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Value   SecretRef `json:"value"`
	Models  []string  `json:"models"`
	Weight  float64   `json:"weight"`
	Status  string    `json:"status"`
	Enabled *bool     `json:"enabled,omitempty"`

	// fields retains the complete redacted object. Bifrost PUT replaces fields,
	// so unknown top-level configuration must be forwarded unchanged.
	fields map[string]json.RawMessage
}

func (k *Key) UnmarshalJSON(data []byte) error {
	type wireKey Key
	var decoded wireKey
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*k = Key(decoded)
	k.fields = fields
	return nil
}

// SecretRef carries the source of a key value (for example env.VAR).
type SecretRef struct {
	Value string `json:"value,omitempty"`
	Ref   string `json:"ref,omitempty"`
	Type  string `json:"type,omitempty"`
}

type keysResponse struct {
	Keys []Key `json:"keys"`
}

// Client calls the Bifrost management API.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient creates a short-timeout client and refuses every redirect. A 302
// could turn a PUT into a false-success GET; 307/308 could replay configuration
// to another origin.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

func (c *Client) Keys() ([]Key, error) {
	return c.KeysContext(context.Background())
}

// KeysContext returns and validates the current provider snapshot.
func (c *Client) KeysContext(ctx context.Context) ([]Key, error) {
	body, err := c.get(ctx, c.baseURL+"/api/providers/"+provider+"/keys")
	if err != nil {
		return nil, err
	}
	var parsed keysResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("réponse bifrost invalide: %w", err)
	}
	for i := range parsed.Keys {
		if err := validateKey(parsed.Keys[i]); err != nil {
			return nil, fmt.Errorf("réponse bifrost invalide (clé %d): %w", i, err)
		}
	}
	return parsed.Keys, nil
}

// KeyContext fetches a fresh single-key snapshot immediately before a PUT.
func (c *Client) KeyContext(ctx context.Context, id string) (Key, error) {
	if id == "" {
		return Key{}, fmt.Errorf("id bifrost vide")
	}
	body, err := c.get(ctx, c.baseURL+"/api/providers/"+provider+"/keys/"+url.PathEscape(id))
	if err != nil {
		return Key{}, err
	}
	var key Key
	if err := json.Unmarshal(body, &key); err != nil {
		return Key{}, fmt.Errorf("réponse bifrost invalide: %w", err)
	}
	if err := validateKey(key); err != nil {
		return Key{}, fmt.Errorf("réponse bifrost invalide: %w", err)
	}
	return key, nil
}

func (c *Client) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bifrost injoignable: %w", err)
	}
	defer resp.Body.Close()
	body, err := readResponseBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("lecture réponse bifrost: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bifrost HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func (c *Client) SetWeight(key Key, weight float64) error {
	return c.SetWeightContext(context.Background(), key, weight)
}

// SetWeightContext changes one weight from a fresh complete env-backed key.
// Plaintext and malformed references are refused rather than risking credential
// replacement in Bifrost's full-object PUT endpoint.
func (c *Client) SetWeightContext(ctx context.Context, key Key, weight float64) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if !managedEnvRef(key.Value) {
		return fmt.Errorf("clé %q non gérée: référence env OPENCODE_GO_API_KEY requise", key.ID)
	}
	if weight < 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		return fmt.Errorf("poids invalide pour %q", key.ID)
	}

	payload := make(map[string]any, len(key.fields)+5)
	for name, value := range key.fields {
		payload[name] = value
	}
	value, err := safeSecretReference(key)
	if err != nil {
		return err
	}
	payload["id"] = key.ID
	payload["name"] = key.Name
	payload["value"] = value
	// Preserve an empty model list. In Bifrost it means "allow none"; changing
	// it to ["*"] would silently broaden access.
	payload["models"] = key.Models
	payload["weight"] = weight

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.baseURL+"/api/providers/"+provider+"/keys/"+url.PathEscape(key.ID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bifrost injoignable: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := readResponseBody(resp.Body)
	if err != nil {
		return fmt.Errorf("lecture réponse bifrost: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bifrost HTTP %d", resp.StatusCode)
	}

	var confirmation struct {
		ID     string   `json:"id"`
		Weight *float64 `json:"weight"`
	}
	if err := json.Unmarshal(respBody, &confirmation); err != nil || confirmation.ID != key.ID ||
		confirmation.Weight == nil || math.Abs(*confirmation.Weight-weight) >= 0.0005 {
		return fmt.Errorf("confirmation bifrost invalide pour %q", key.ID)
	}
	return nil
}

func safeSecretReference(key Key) (map[string]any, error) {
	value := make(map[string]any)
	if raw, ok := key.fields["value"]; ok {
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("value bifrost invalide pour %q", key.ID)
		}
	}
	// Never send the redacted preview. Preserve future non-secret nested fields.
	delete(value, "value")
	value["ref"] = key.Value.Ref
	value["type"] = key.Value.Type
	return value, nil
}

func managedEnvRef(value SecretRef) bool {
	if value.Type != "env" {
		return false
	}
	const prefix = "env.OPENCODE_GO_API_KEY"
	return value.Ref == prefix || (strings.HasPrefix(value.Ref, prefix+"_") && len(value.Ref) > len(prefix)+1)
}

func validateKey(key Key) error {
	switch {
	case key.ID == "":
		return fmt.Errorf("id manquant")
	case key.Name == "":
		return fmt.Errorf("nom manquant pour %q", key.ID)
	case key.Status == "":
		return fmt.Errorf("status manquant pour %q", key.ID)
	case math.IsNaN(key.Weight) || math.IsInf(key.Weight, 0) || key.Weight < 0:
		return fmt.Errorf("poids invalide pour %q", key.ID)
	default:
		return nil
	}
}

func readResponseBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBody {
		return nil, fmt.Errorf("réponse trop volumineuse (limite %d octets)", maxResponseBody)
	}
	return body, nil
}
