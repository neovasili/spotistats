package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neovasili/spotistats/internal/spotify"
)

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{EnvRegion, EnvTableName, EnvDDBEndpoint, EnvSSMPrefix,
		EnvTokenFile, EnvTimezone, EnvClientID, EnvClientSecret, EnvRedirectURI,
		EnvCaptureLimit, EnvLogLevel, "AWS_REGION"} {
		t.Setenv(k, "")
	}

	c := Load()
	if c.SSMPrefix != DefaultSSMPrefix {
		t.Errorf("SSMPrefix = %q, want %q", c.SSMPrefix, DefaultSSMPrefix)
	}
	if c.Timezone != DefaultTimezone {
		t.Errorf("Timezone = %q, want %q", c.Timezone, DefaultTimezone)
	}
	// Spotify forbids `localhost`; the default must be the loopback IP literal.
	if c.RedirectURI != "http://127.0.0.1:8888/callback" {
		t.Errorf("RedirectURI = %q, want the loopback IP literal", c.RedirectURI)
	}
	if c.CaptureLimit != spotify.MaxRecentlyPlayedLimit {
		t.Errorf("CaptureLimit = %d, want %d", c.CaptureLimit, spotify.MaxRecentlyPlayedLimit)
	}
}

func TestLoadClampsCaptureLimit(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"0", spotify.MaxRecentlyPlayedLimit},
		{"-5", spotify.MaxRecentlyPlayedLimit},
		{"51", spotify.MaxRecentlyPlayedLimit},
		{"1000", spotify.MaxRecentlyPlayedLimit},
		{"garbage", spotify.MaxRecentlyPlayedLimit},
		{"10", 10},
		{"50", 50},
	} {
		t.Setenv(EnvCaptureLimit, tc.in)
		if got := Load().CaptureLimit; got != tc.want {
			t.Errorf("CaptureLimit for %q = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSSMPrefixTrailingSlashTrimmed(t *testing.T) {
	t.Setenv(EnvSSMPrefix, "/custom/prefix/")
	c := Load()
	if c.RefreshTokenParam() != "/custom/prefix/refresh_token" {
		t.Errorf("RefreshTokenParam = %q", c.RefreshTokenParam())
	}
}

func TestParamNames(t *testing.T) {
	c := Config{SSMPrefix: DefaultSSMPrefix}
	// These must match the paths docs/PREREQUISITES.md step 5 tells the operator to create.
	if got := c.ClientIDParam(); got != "/spotistats/spotify/client_id" {
		t.Errorf("ClientIDParam = %q", got)
	}
	if got := c.ClientSecretParam(); got != "/spotistats/spotify/client_secret" {
		t.Errorf("ClientSecretParam = %q", got)
	}
	if got := c.RefreshTokenParam(); got != "/spotistats/spotify/refresh_token" {
		t.Errorf("RefreshTokenParam = %q", got)
	}
}

// TestRegionResolution is the regression test for a production outage.
//
// The region used to fall back to a hardcoded "us-east-1". When the deployment moved to
// eu-west-1 that constant was left behind, and because the Lambdas deliberately do NOT set
// SPOTISTATS_REGION -- relying on the runtime-provided AWS_REGION -- every function silently
// addressed a region with no table in it. Every DynamoDB call returned
// ResourceNotFoundException and the dashboard served a 403.
//
// There is now no hardcoded default at all: AWS_REGION is honoured, and an empty region means
// "let the SDK resolve it from the profile".
func TestRegionResolution(t *testing.T) {
	t.Run("AWS_REGION is honoured, as the Lambda runtime provides it", func(t *testing.T) {
		t.Setenv(EnvRegion, "")
		t.Setenv("AWS_REGION", "eu-west-1")
		if got := Load().Region; got != "eu-west-1" {
			t.Errorf("Region = %q, want eu-west-1 from AWS_REGION", got)
		}
	})

	t.Run("an explicit SPOTISTATS_REGION wins", func(t *testing.T) {
		t.Setenv(EnvRegion, "eu-central-1")
		t.Setenv("AWS_REGION", "eu-west-1")
		if got := Load().Region; got != "eu-central-1" {
			t.Errorf("Region = %q, want the explicit override", got)
		}
	})

	t.Run("no hardcoded fallback: empty means let the SDK resolve", func(t *testing.T) {
		t.Setenv(EnvRegion, "")
		t.Setenv("AWS_REGION", "")
		if got := Load().Region; got != "" {
			t.Errorf("Region = %q, want empty. A hardcoded default duplicates cdk.json and "+
				"drifted from it once already, taking the whole deployment down", got)
		}
	})

	t.Run("a missing region does not fail validation", func(t *testing.T) {
		// The SDK resolves it from the profile, so an empty value here is legitimate.
		c := Config{TableName: "t", Timezone: "Europe/Madrid"}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil for an unset region", err)
		}
	})
}

func TestValidate(t *testing.T) {
	base := Config{Region: "eu-west-1", TableName: "t", Timezone: "Europe/Madrid"}
	if err := base.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}

	noTable := base
	noTable.TableName = ""
	if err := noTable.Validate(); err == nil {
		t.Error("missing table name accepted")
	}

	badTZ := base
	badTZ.Timezone = "Not/AZone"
	if err := badTZ.Validate(); err == nil {
		t.Error("invalid timezone accepted")
	}
}

func TestRedactedHidesSecret(t *testing.T) {
	c := Config{ClientID: "public-id", ClientSecret: "super-secret-value"}
	r := c.Redacted()
	if r.ClientSecret == c.ClientSecret {
		t.Error("Redacted did not replace the secret")
	}
	// A prefix of a short secret is still most of the secret, so nothing of it may survive.
	if r.ClientSecret != "[redacted]" {
		t.Errorf("ClientSecret = %q, want a fixed placeholder", r.ClientSecret)
	}
	if r.ClientID != "public-id" {
		t.Error("Redacted altered the non-secret client ID")
	}
}

// TestRedactedCoversEverySecret is the guard that matters more than the case above.
//
// Redacted() originally covered ClientSecret alone, so `spotistats config` printed the
// TheAudioDB key in the clear for as long as that key existed. The failure mode is silent by
// construction: adding a credential field is a one-line change, and nothing about it suggests
// that a second function needs editing too.
//
// So this enumerates the struct reflectively. A new field whose name looks like a credential
// fails the build until it is either redacted or explicitly listed as public.
func TestRedactedCoversEverySecret(t *testing.T) {
	// Fields that legitimately survive redaction, with the reason. A client ID and a contact
	// URL are public by design; a webhook URL, an API key and a secret are not.
	public := map[string]bool{
		"ClientID":           true, // public half of the OAuth pair
		"MusicBrainzContact": true, // deliberately identifying: MusicBrainz requires it
		"TokenURL":           true, // Spotify's public token endpoint, not a token
		"TokenFile":          true, // a path on disk, not the token it holds
	}
	secretish := func(name string) bool {
		lower := strings.ToLower(name)
		for _, marker := range []string{"secret", "key", "token", "webhook", "password", "credential"} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
		return false
	}

	// Fill every string field with a distinctive value, so a survivor is unmistakable.
	var c Config
	v := reflect.ValueOf(&c).Elem()
	typ := v.Type()
	const canary = "CANARY-VALUE-DO-NOT-LEAK"
	for i := range typ.NumField() {
		if v.Field(i).Kind() == reflect.String && v.Field(i).CanSet() {
			v.Field(i).SetString(canary)
		}
	}

	r := reflect.ValueOf(c.Redacted())
	var leaked []string
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if v.Field(i).Kind() != reflect.String || public[name] || !secretish(name) {
			continue
		}
		if r.Field(i).String() == canary {
			leaked = append(leaked, name)
		}
	}
	if len(leaked) > 0 {
		t.Errorf("Redacted() left these credential fields intact, so `spotistats config` "+
			"and any log of a Config will print them: %v", leaked)
	}
}

// ---------------------------------------------------------------------------
// FileTokenStore
// ---------------------------------------------------------------------------

func TestFileTokenStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "token.json")
	s := NewFileTokenStore(path)
	ctx := context.Background()

	if _, err := s.Get(ctx); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("Get on a missing file = %v, want ErrTokenNotFound", err)
	}

	if err := s.Put(ctx, "refresh-1"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "refresh-1" {
		t.Errorf("Get = %q, want refresh-1", got)
	}

	// Rotation overwrites.
	if err := s.Put(ctx, "refresh-2"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Get(ctx); got != "refresh-2" {
		t.Errorf("after rotation Get = %q, want refresh-2", got)
	}
}

// The file holds a live credential, so it must not be group- or world-readable.
func TestFileTokenStorePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "token.json")
	s := NewFileTokenStore(path)
	if err := s.Put(context.Background(), "refresh-1"); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %04o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", perm)
	}
}

// A half-written token file is indistinguishable from a revoked one, so writes must be
// atomic and must leave no temp files behind.
func TestFileTokenStoreWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	s := NewFileTokenStore(path)

	for i := 0; i < 3; i++ {
		if err := s.Put(context.Background(), "refresh"); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the token file (temp files must be cleaned up)", names)
	}
}

// Someone hand-editing the file is most likely to leave a bare token, so accept that.
func TestFileTokenStoreAcceptsBareToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte("  raw-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := NewFileTokenStore(path).Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "raw-token-value" {
		t.Errorf("Get = %q, want the trimmed bare token", got)
	}
}

func TestFileTokenStoreRejectsEmptyAndMalformed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "token.json")
	s := NewFileTokenStore(path)

	if err := s.Put(ctx, ""); err == nil {
		t.Error("Put accepted an empty token; that would silently destroy a working one")
	}
	if err := os.WriteFile(path, []byte(`{"refresh_token":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("Get on an empty token = %v, want ErrTokenNotFound", err)
	}
	if err := os.WriteFile(path, []byte(`{ this is not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx); err == nil {
		t.Error("Get accepted malformed JSON")
	}
}

func TestResolveTokenStoreSelectsFileWhenConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	c := Config{TokenFile: path, Region: "us-east-1"}
	if !c.UsesLocalTokenFile() {
		t.Fatal("UsesLocalTokenFile = false despite TokenFile being set")
	}
	ts, err := c.ResolveTokenStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fs, ok := ts.(*FileTokenStore)
	if !ok {
		t.Fatalf("ResolveTokenStore = %T, want *FileTokenStore", ts)
	}
	if fs.Path() != path {
		t.Errorf("Path = %q, want %q", fs.Path(), path)
	}
}

func TestResolveCredentialsFromEnv(t *testing.T) {
	c := Config{ClientID: "id", ClientSecret: "secret"}
	// With both present there must be no AWS call at all, so this passes with no creds.
	got, err := c.ResolveCredentials(context.Background())
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if got.ClientID != "id" || got.ClientSecret != "secret" {
		t.Errorf("credentials = %+v", got)
	}
}

// The file store must satisfy the interface internal/spotify defines, which is the seam
// that keeps that package AWS-free.
func TestStoresSatisfyInterface(t *testing.T) {
	var _ spotify.RefreshTokenStore = NewFileTokenStore("x")
	var _ spotify.RefreshTokenStore = NewSSMTokenStore(nil, "x")
}
