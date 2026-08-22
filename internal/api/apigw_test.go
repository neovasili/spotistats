package api_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/neovasili/spotistats/internal/api"
)

func gwRequest(method, rawPath, rawQuery string) events.APIGatewayV2HTTPRequest {
	return events.APIGatewayV2HTTPRequest{
		RawPath:        rawPath,
		RawQueryString: rawQuery,
		Headers:        map[string]string{"accept": "application/json"},
		RequestContext: events.APIGatewayV2HTTPRequestContext{
			HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{
				Method: method, Path: rawPath, SourceIP: "203.0.113.7",
			},
		},
	}
}

// TestAPIGatewayAdapterMatchesDirectServing is the property that makes the offline frontend
// loop trustworthy: a request routed through the Lambda adapter must produce the same result
// as the same request served directly.
func TestAPIGatewayAdapterMatchesDirectServing(t *testing.T) {
	h := newAPI(t)

	for _, tc := range []struct{ path, query string }{
		{api.BasePath + "/meta", ""},
		{api.BasePath + "/stats", "dim=artist&id=ar1&period=2026"},
		{api.BasePath + "/top", "dim=track&period=ALL&limit=3"},
		{api.BasePath + "/timeline", "from=2025-12&to=2026-02&bucket=month"},
	} {
		t.Run(tc.path+"?"+tc.query, func(t *testing.T) {
			gw, err := api.ServeAPIGateway(context.Background(), h,
				gwRequest(http.MethodGet, tc.path, tc.query))
			if err != nil {
				t.Fatalf("adapter: %v", err)
			}

			target := tc.path
			if tc.query != "" {
				target += "?" + tc.query
			}
			direct := get(t, h, strings.TrimPrefix(target, api.BasePath))

			if gw.StatusCode != direct.Code {
				t.Errorf("status: adapter %d, direct %d", gw.StatusCode, direct.Code)
			}
			if gw.IsBase64Encoded {
				t.Error("JSON must travel as text, not base64")
			}
			// Compare decoded JSON rather than bytes, so key order cannot cause a false failure.
			var a, b any
			if err := json.Unmarshal([]byte(gw.Body), &a); err != nil {
				t.Fatalf("adapter body is not JSON: %v\n%s", err, gw.Body)
			}
			if err := json.Unmarshal(direct.Body.Bytes(), &b); err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(a, b) {
				t.Errorf("bodies differ\nadapter: %s\ndirect:  %s", gw.Body, direct.Body.String())
			}
			// The cache headers are what make edge caching work, so they must survive.
			if got := gw.Headers["Cache-Control"]; got != direct.Header().Get("Cache-Control") {
				t.Errorf("Cache-Control: adapter %q, direct %q", got, direct.Header().Get("Cache-Control"))
			}
			if got := gw.Headers["Content-Type"]; !strings.Contains(got, "application/json") {
				t.Errorf("Content-Type = %q", got)
			}
		})
	}
}

func TestAPIGatewayAdapterPropagatesErrors(t *testing.T) {
	h := newAPI(t)
	gw, err := api.ServeAPIGateway(context.Background(), h,
		gwRequest(http.MethodGet, api.BasePath+"/stats", "dim=nonsense&id=x"))
	if err != nil {
		t.Fatalf("adapter returned a Go error; a 4xx must travel as a response: %v", err)
	}
	if gw.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", gw.StatusCode)
	}
	if got := gw.Headers["Cache-Control"]; got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(gw.Body), &env); err != nil {
		t.Fatalf("error body is not the envelope: %v", err)
	}
	if env.Error.Code != api.CodeInvalidDimension {
		t.Errorf("code = %q", env.Error.Code)
	}
}

// Payload format 2.0 may omit RawQueryString and supply only the decoded map.
func TestAPIGatewayAdapterFallsBackToDecodedQuery(t *testing.T) {
	h := newAPI(t)
	req := gwRequest(http.MethodGet, api.BasePath+"/stats", "")
	req.QueryStringParameters = map[string]string{
		"dim": "artist", "id": "ar1", "period": "2026",
	}

	gw, err := api.ServeAPIGateway(context.Background(), h, req)
	if err != nil {
		t.Fatal(err)
	}
	if gw.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", gw.StatusCode, gw.Body)
	}
	var out api.StatsResponse
	if err := json.Unmarshal([]byte(gw.Body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Metrics.Plays != 2 {
		t.Errorf("plays = %d, want 2", out.Metrics.Plays)
	}
}

func TestAPIGatewayAdapterRejectsWriteMethods(t *testing.T) {
	h := newAPI(t)
	gw, err := api.ServeAPIGateway(context.Background(), h,
		gwRequest(http.MethodPost, api.BasePath+"/meta", ""))
	if err != nil {
		t.Fatal(err)
	}
	if gw.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", gw.StatusCode)
	}
	if got := gw.Headers["Allow"]; got != "GET, HEAD" {
		t.Errorf("Allow = %q", got)
	}
}

// A base64 request body must be decoded rather than passed through as its encoding.
func TestAPIGatewayAdapterDecodesBase64Body(t *testing.T) {
	h := newAPI(t)
	req := gwRequest(http.MethodGet, api.BasePath+"/meta", "")
	req.Body = base64.StdEncoding.EncodeToString([]byte("ignored"))
	req.IsBase64Encoded = true

	gw, err := api.ServeAPIGateway(context.Background(), h, req)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if gw.StatusCode != http.StatusOK {
		t.Errorf("status = %d", gw.StatusCode)
	}
}

func TestAPIGatewayAdapterUnknownPath(t *testing.T) {
	h := newAPI(t)
	gw, err := api.ServeAPIGateway(context.Background(), h,
		gwRequest(http.MethodGet, api.BasePath+"/nope", ""))
	if err != nil {
		t.Fatal(err)
	}
	if gw.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", gw.StatusCode)
	}
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}
