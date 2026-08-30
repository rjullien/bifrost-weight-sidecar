// Package bifrost reads provider key state from the Bifrost gateway and
// updates key weights through its management API.
package bifrost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// provider is the only provider this sidecar manages.
const provider = "opencode-go"

// Key is one provider key as exposed by GET /api/providers/{provider}/keys.
type Key struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Value  SecretRef `json:"value"`
	Models []string  `json:"models"`
	Weight float64   `json:"weight"`
	Status string    `json:"status"`
}

// SecretRef carries the source of a key value ("env.VAR"). The resolved
// secret is never sent back: an update only re-states the reference.
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

// NewClient creates a Client for the given base URL. The timeout must stay
// short: a dead Bifrost must not stall the sidecar loop.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// Keys returns the current opencode-go provider keys with their weights and
// status.
func (c *Client) Keys() ([]Key, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/api/providers/"+provider+"/keys", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bifrost injoignable: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("lecture réponse bifrost: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bifrost HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var parsed keysResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("réponse bifrost invalide: %w", err)
	}
	return parsed.Keys, nil
}

// SetWeight changes the load-balancing weight of one key. The payload mirrors
// what the Bifrost UI sends: the full key object with its env reference (never
// the masked secret preview) and its model list — the update endpoint replaces
// fields rather than merging them, so a partial payload would wipe them.
func (c *Client) SetWeight(key Key, weight float64) error {
	models := key.Models
	if len(models) == 0 {
		models = []string{"*"}
	}

	payload := map[string]any{
		"id":     key.ID,
		"name":   key.Name,
		"value":  SecretRef{Ref: key.Value.Ref, Type: key.Value.Type},
		"models": models,
		"weight": weight,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut,
		c.baseURL+"/api/providers/"+provider+"/keys/"+key.ID, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bifrost injoignable: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("lecture réponse bifrost: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bifrost HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
