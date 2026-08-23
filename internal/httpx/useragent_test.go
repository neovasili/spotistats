package httpx_test

import (
	"errors"
	"testing"

	"github.com/neovasili/spotistats/internal/httpx"
)

// TestRequireUserAgent guards a rule that comes from the wire, not from taste: MusicBrainz
// mandates a descriptive User-Agent with contact information, and throttles anonymous or
// default library agents far harder as a class. A client that silently sent a default would
// pass every test and be throttled into uselessness in production.
func TestRequireUserAgent(t *testing.T) {
	valid := []string{
		"spotistats/1.0 ( https://spotistats.neovasili.com )",
		"spotistats/1.0 (me@example.com)",
		"myapp/2 https://example.com/contact",
	}
	for _, ua := range valid {
		if err := httpx.RequireUserAgent(ua); err != nil {
			t.Errorf("RequireUserAgent(%q) = %v, want nil", ua, err)
		}
	}

	invalid := map[string]string{
		"empty":                "",
		"whitespace":           "   ",
		"no contact":           "spotistats/1.0",
		"default python agent": "Python-urllib/3.11",
		"default Go agent":     "Go-http-client/2.0",
		"curl":                 "curl/8.4.0 (https://curl.se)",
	}
	for name, ua := range invalid {
		err := httpx.RequireUserAgent(ua)
		if err == nil {
			t.Errorf("%s: RequireUserAgent(%q) = nil, want an error", name, ua)
			continue
		}
		if !errors.Is(err, httpx.ErrUserAgentRequired) {
			t.Errorf("%s: error does not wrap ErrUserAgentRequired: %v", name, err)
		}
	}
}
