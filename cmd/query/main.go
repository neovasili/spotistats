// Command query is the Lambda behind /api/* on CloudFront.
//
// It is a thin adapter: the handler in internal/api is a plain http.Handler, and
// `spotistats serve` binds the same handler to a local port. Keeping the two on identical
// code is what makes the offline frontend loop (docs/SPECS.md 7.4) faithful.
package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/neovasili/spotistats/internal/api"
	"github.com/neovasili/spotistats/internal/config"
)

// Built once per container and reused. A failed build is not cached, so a transient DynamoDB
// or configuration problem is retried on the next invocation rather than poisoning the
// container for its lifetime.
var (
	mu      sync.Mutex
	handler http.Handler
)

func main() {
	lambda.Start(handle)
}

func handle(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	h, err := getHandler(ctx)
	if err != nil {
		// Returning an error would surface as a 502 with no useful body. A 500 in the
		// documented envelope is more use to a caller, and the detail is in the log.
		return events.APIGatewayV2HTTPResponse{
			StatusCode: http.StatusInternalServerError,
			Headers: map[string]string{
				"Content-Type":  "application/json; charset=utf-8",
				"Cache-Control": "no-store",
			},
			Body: `{"error":{"code":"INTERNAL","message":"service unavailable"}}` + "\n",
		}, nil
	}
	return api.ServeAPIGateway(ctx, h, req)
}

func getHandler(ctx context.Context) (http.Handler, error) {
	mu.Lock()
	defer mu.Unlock()

	if handler != nil {
		return handler, nil
	}

	cfg := config.Load()
	deps, err := config.Build(ctx, cfg, config.BuildOptions{
		NeedStore: true,
		// Deliberately NOT verifying the store config here. VerifyConfig writes the row when
		// absent, and the query Lambda is granted read-only DynamoDB access (docs/SPECS.md
		// 10.1) precisely so the only internet-reachable component cannot mutate anything.
		VerifyStoreConfig: false,
	})
	if err != nil {
		return nil, fmt.Errorf("query: build dependencies: %w", err)
	}

	cal, err := cfg.Calendar()
	if err != nil {
		return nil, err
	}

	h, err := api.New(api.Config{
		Store:    deps.Store,
		Calendar: cal,
		Logger:   deps.Logger,
	})
	if err != nil {
		return nil, err
	}

	handler = h
	return handler, nil
}
