package gh

import "net/http"

// NewTestClient builds a client aimed at a local test server.
//
// It exists so packages layered above gh can assert their own request
// behaviour — which calls they make, and how many — without reaching the
// network. That matters because the account shares one REST budget across the
// daemon and every agent, so "does this code path write?" is a correctness
// property worth a test, not just a performance note.
//
// Retries and backoff are wound down to keep tests fast; everything else,
// including the ETag/conditional-GET cache, behaves exactly as in production.
func NewTestClient(baseURL string, client *http.Client) *GitHub {
	if client == nil {
		client = http.DefaultClient
	}
	return &GitHub{
		token:       "test-token",
		httpClient:  client,
		apiBase:     baseURL,
		graphBase:   baseURL + "/graphql",
		maxRetries:  1,
		maxWait:     0,
		backoffBase: 0,
	}
}
