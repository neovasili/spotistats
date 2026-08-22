package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// ServeAPIGateway runs an http.Handler against an API Gateway HTTP API (payload format 2.0)
// request and converts the result back.
//
// Hand-rolled rather than pulled from a proxy library for one reason that matters: it keeps
// the Lambda and `spotistats serve` on byte-identical handler code, so the offline frontend
// loop (docs/SPECS.md 7.4) exercises production behaviour rather than an approximation. The
// conversion is small because the API is GET-only JSON.
func ServeAPIGateway(
	ctx context.Context, h http.Handler, req events.APIGatewayV2HTTPRequest,
) (events.APIGatewayV2HTTPResponse, error) {
	httpReq, err := toHTTPRequest(ctx, req)
	if err != nil {
		return events.APIGatewayV2HTTPResponse{}, err
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httpReq)
	return fromRecorder(rec), nil
}

func toHTTPRequest(ctx context.Context, req events.APIGatewayV2HTTPRequest) (*http.Request, error) {
	method := req.RequestContext.HTTP.Method
	if method == "" {
		method = http.MethodGet
	}

	path := req.RawPath
	if path == "" {
		path = req.RequestContext.HTTP.Path
	}
	if path == "" {
		path = "/"
	}

	target := path
	if req.RawQueryString != "" {
		target += "?" + req.RawQueryString
	} else if len(req.QueryStringParameters) > 0 {
		// RawQueryString is normally present; this is the fallback for a payload that only
		// supplied the decoded map. Values are re-encoded so the handler's own parsing is the
		// single source of truth.
		q := url.Values{}
		for k, v := range req.QueryStringParameters {
			q.Set(k, v)
		}
		target += "?" + q.Encode()
	}

	var body io.Reader = http.NoBody
	if req.Body != "" {
		if req.IsBase64Encoded {
			raw, derr := base64.StdEncoding.DecodeString(req.Body)
			if derr != nil {
				return nil, derr
			}
			body = bytes.NewReader(raw)
		} else {
			body = strings.NewReader(req.Body)
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}

	for k, v := range req.Headers {
		// Payload format 2.0 joins repeated headers with a comma, which is how net/http
		// represents them too.
		for _, part := range strings.Split(v, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				httpReq.Header.Add(k, trimmed)
			}
		}
	}
	if len(req.Cookies) > 0 {
		httpReq.Header.Set("Cookie", strings.Join(req.Cookies, "; "))
	}

	// The source address matters only for logging; the API is unauthenticated.
	if ip := req.RequestContext.HTTP.SourceIP; ip != "" {
		httpReq.RemoteAddr = ip + ":0"
	}

	return httpReq, nil
}

func fromRecorder(rec *httptest.ResponseRecorder) events.APIGatewayV2HTTPResponse {
	out := events.APIGatewayV2HTTPResponse{
		StatusCode: rec.Code,
		Headers:    make(map[string]string, len(rec.Header())),
	}

	for k, vs := range rec.Header() {
		if len(vs) == 0 {
			continue
		}
		if len(vs) == 1 {
			out.Headers[k] = vs[0]
			continue
		}
		// Repeated headers are joined; MultiValueHeaders does not exist in payload format 2.0.
		out.Headers[k] = strings.Join(vs, ",")
	}

	body := rec.Body.Bytes()
	// The API only ever emits UTF-8 JSON, so base64 would just inflate the payload. Detect
	// rather than assume: a future binary response would otherwise be silently corrupted.
	if isTextual(body) {
		out.Body = string(body)
	} else {
		out.Body = base64.StdEncoding.EncodeToString(body)
		out.IsBase64Encoded = true
	}
	return out
}

// isTextual reports whether b can travel as a plain JSON string. A NUL byte is the cheap,
// reliable signal that it cannot.
func isTextual(b []byte) bool {
	return !bytes.ContainsRune(b, 0)
}
