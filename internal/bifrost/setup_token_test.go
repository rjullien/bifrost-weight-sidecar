package bifrost

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSetupTokenHeader(t *testing.T) {
	for _, tc := range []struct {
		name, token, want string
		sent              bool
	}{
		{"unset", "", "", false},
		{"set", " s3cret ", "s3cret", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			var present []bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, ok := r.Header[http.CanonicalHeaderKey(SetupTokenHeader)]
				present = append(present, ok)
				got = append(got, r.Header.Get(SetupTokenHeader))
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodGet:
					if r.URL.Path == "/api/providers/opencode-go/keys" {
						_, _ = w.Write([]byte(`{"keys":[{"id":"k1","name":"k1","status":"ok","weight":1,"value":{"type":"env","ref":"env.OPENCODE_GO_API_KEY"}}]}`))
						return
					}
					_, _ = w.Write([]byte(`{"id":"k1","name":"k1","status":"ok","weight":1,"value":{"type":"env","ref":"env.OPENCODE_GO_API_KEY"}}`))
				case http.MethodPut:
					_, _ = w.Write([]byte(`{"id":"k1","weight":2}`))
				}
			}))
			defer srv.Close()
			c := NewClient(srv.URL, 2*time.Second)
			c.SetSetupToken(tc.token)
			keys, err := c.Keys()
			if err != nil {
				t.Fatal(err)
			}
			k, err := c.KeyContext(t.Context(), "k1")
			if err != nil {
				t.Fatal(err)
			}
			if err := c.SetWeight(k, 2); err != nil {
				t.Fatal(err)
			}
			if len(keys) != 1 || len(got) != 3 {
				t.Fatalf("unexpected calls: %d", len(got))
			}
			for i := range got {
				if present[i] != tc.sent || got[i] != tc.want {
					t.Fatalf("call %d: header present=%v value=%q", i, present[i], got[i])
				}
			}
		})
	}
}
