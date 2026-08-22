// Package storetest provides the DynamoDB Local test harness for internal/store.
//
// Integration tests that use it must live in `package store_test` (the external test
// package), because this package imports internal/store and an internal test file would
// create an import cycle. That is a feature: it forces the integration suite to exercise
// the exported API.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// The image is pinned rather than tracking latest so a silent upstream change cannot turn
// the suite red for reasons unrelated to the code under test.
const dynamoLocalImage = "amazon/dynamodb-local:3.3.1"

// Environment variables controlling the harness.
const (
	// EnvEndpoint points the suite at an already-running DynamoDB Local, bypassing
	// testcontainers entirely. CI service containers and hand-run containers use this.
	EnvEndpoint = "SPOTISTATS_TEST_DDB_ENDPOINT"

	// EnvRequire turns "Docker is unavailable" from a skip into a failure. CI sets it so a
	// broken Docker daemon can never silently skip the whole integration suite and report
	// green.
	EnvRequire = "SPOTISTATS_TEST_REQUIRE_DDB"
)

var (
	sharedOnce      sync.Once
	sharedEndpoint  string
	sharedErr       error
	sharedContainer testcontainers.Container
)

// Shutdown terminates the shared DynamoDB Local container. A consuming package must call
// it from TestMain:
//
//	func TestMain(m *testing.M) {
//	    code := m.Run()
//	    storetest.Shutdown()
//	    os.Exit(code)
//	}
//
// It is required because the harness disables Ryuk, testcontainers' reaper sidecar -- see
// startContainer for why. Without this call the container would outlive the test binary.
func Shutdown() {
	if sharedContainer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sharedContainer.Terminate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "storetest: terminate container: %v\n", err)
	}
	sharedContainer = nil
}

// RequireDynamoDB returns a DynamoDB client pointed at a running DynamoDB Local, or skips
// the test.
//
// The resolution order is deliberate:
//
//  1. -short          -> skip. This is what makes `make test-short` work with no Docker.
//  2. EnvEndpoint set -> use it verbatim.
//  3. Docker reachable-> start (once per package) and reuse a container.
//  4. otherwise       -> skip, UNLESS EnvRequire is set, in which case fail.
//
// Runtime probing is used rather than a build tag because tagged code is excluded from the
// default build and rots: refactors and `go vet` never see it.
func RequireDynamoDB(t *testing.T) *dynamodb.Client {
	t.Helper()

	if testing.Short() {
		t.Skip("storetest: skipping DynamoDB Local test in -short mode")
	}

	if ep := os.Getenv(EnvEndpoint); ep != "" {
		return newClient(t, ep)
	}

	sharedOnce.Do(func() { sharedEndpoint, sharedErr = startContainer() })

	if sharedErr != nil {
		if os.Getenv(EnvRequire) != "" {
			t.Fatalf("storetest: %s is set but DynamoDB Local could not be started: %v",
				EnvRequire, sharedErr)
		}
		t.Skipf("storetest: skipping, DynamoDB Local unavailable (set %s=1 to make this fatal): %v",
			EnvRequire, sharedErr)
	}
	return newClient(t, sharedEndpoint)
}

// startContainer launches DynamoDB Local and returns its endpoint.
func startContainer() (string, error) {
	host := resolveDockerHost()
	if host == "" {
		return "", errors.New("no Docker daemon answered on any known socket " +
			"(tried /var/run, colima, Rancher Desktop, Docker Desktop); set DOCKER_HOST to override")
	}
	// testcontainers reads DOCKER_HOST from the environment and its own detection would
	// otherwise prefer a stale Docker Desktop socket over the live daemon found above.
	if os.Getenv("DOCKER_HOST") == "" {
		_ = os.Setenv("DOCKER_HOST", host)
	}

	// Ryuk is testcontainers' reaper sidecar: it watches the test process and removes
	// containers if it dies abnormally. It cannot run under colima -- it bind-mounts the
	// Docker socket at a path that does not exist inside the VM, so it exits immediately
	// and every container start blocks for a 60s readiness timeout before failing.
	//
	// Disabling it moves lifecycle management to Shutdown(), which TestMain must call. The
	// residual risk is that a SIGKILLed test binary leaks one container; it runs with
	// -inMemory so it costs a few MB, and `docker rm` clears it.
	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "" {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}

	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        dynamoLocalImage,
		ExposedPorts: []string{"8000/tcp"},
		// -inMemory keeps everything in RAM (tests create and drop tables constantly) and
		// -sharedDb puts every "credential set" in one database, so the arbitrary static
		// credentials below all address the same tables.
		Cmd: []string{"-jar", "DynamoDBLocal.jar", "-inMemory", "-sharedDb"},
		// DynamoDB Local answers GET / with HTTP 400, not 200: it is a JSON-RPC endpoint
		// and a bare GET is a malformed request. A default 200 matcher waits forever.
		WaitingFor: wait.ForHTTP("/").
			WithPort("8000/tcp").
			WithStatusCodeMatcher(func(status int) bool { return status == 400 }).
			WithStartupTimeout(90 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return "", fmt.Errorf("start %s: %w", dynamoLocalImage, err)
	}
	sharedContainer = container

	ctrHost, err := container.Host(ctx)
	if err != nil {
		return "", fmt.Errorf("container host: %w", err)
	}
	port, err := container.MappedPort(ctx, "8000/tcp")
	if err != nil {
		return "", fmt.Errorf("mapped port: %w", err)
	}
	return fmt.Sprintf("http://%s:%s", ctrHost, port.Port()), nil
}

// resolveDockerHost finds a Docker daemon that actually answers and returns a DOCKER_HOST
// value for it, or "" if none does.
//
// This exists because socket-file presence is not evidence of a running daemon. A machine
// that once ran Docker Desktop keeps a stale ~/.docker/run/docker.sock that accepts a
// connection and then never replies, and testcontainers' own host detection picks it ahead
// of the active docker context -- so a working colima or Rancher install gets reported as
// "Cannot connect to the Docker daemon". Pinging /_ping over each candidate distinguishes
// a live daemon from a leftover socket.
func resolveDockerHost() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		// Explicit configuration wins; testcontainers will report any problem with it.
		return h
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		"/var/run/docker.sock",                // Linux, and CI
		home + "/.colima/default/docker.sock", // colima
		home + "/.rd/docker.sock",             // Rancher Desktop
		home + "/.docker/run/docker.sock",     // Docker Desktop, checked last: most often stale
	}
	for _, sock := range candidates {
		if home == "" && strings.HasPrefix(sock, "/") && !strings.HasPrefix(sock, "/var") {
			continue
		}
		if pingDocker(sock) {
			return "unix://" + sock
		}
	}
	return ""
}

// pingDocker reports whether a live Docker daemon answers on the given unix socket.
func pingDocker(socket string) bool {
	if fi, err := os.Stat(socket); err != nil || fi.Mode()&os.ModeSocket == 0 {
		return false
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
	req, err := http.NewRequest(http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// newClient builds a DynamoDB client for endpoint.
//
// It NEVER calls config.LoadDefaultConfig. Static throwaway credentials and an explicit
// endpoint mean the developer's real AWS profile, AWS_PROFILE, SSO caches and IMDS are all
// irrelevant -- which is what lets this suite run with expired or absent AWS credentials.
func newClient(t *testing.T, endpoint string) *dynamodb.Client {
	t.Helper()
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}
	return dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})
}

// Endpoint returns the endpoint of the shared DynamoDB Local instance, starting it if
// needed. It exists so a test outside this package -- the CLI end-to-end test, which points
// the application at the same container via configuration -- can reach it.
func Endpoint(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("storetest: skipping DynamoDB Local test in -short mode")
	}
	if ep := os.Getenv(EnvEndpoint); ep != "" {
		return ep
	}
	sharedOnce.Do(func() { sharedEndpoint, sharedErr = startContainer() })
	if sharedErr != nil {
		if os.Getenv(EnvRequire) != "" {
			t.Fatalf("storetest: %s is set but DynamoDB Local could not be started: %v",
				EnvRequire, sharedErr)
		}
		t.Skipf("storetest: skipping, DynamoDB Local unavailable: %v", sharedErr)
	}
	return sharedEndpoint
}
