package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/neovasili/spotistats/internal/spotify"
)

// SSMAPI is the subset of the SSM client used here, so the store is testable without AWS.
type SSMAPI interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	PutParameter(context.Context, *ssm.PutParameterInput, ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
}

// ErrTokenNotFound means no refresh token has been stored yet, so the interactive
// authorisation flow has not been run.
var ErrTokenNotFound = errors.New("config: no refresh token stored; run `spotistats auth login`")

// SSMTokenStore keeps the refresh token in an SSM SecureString parameter.
//
// Parameter Store is used rather than Secrets Manager because it is functionally
// equivalent here and free, whereas Secrets Manager bills per secret per month. Its main
// advantage -- automatic rotation -- does not apply: Spotify decides when to rotate a
// refresh token, and that is handled in the token source.
type SSMTokenStore struct {
	client SSMAPI
	name   string
}

var _ spotify.RefreshTokenStore = (*SSMTokenStore)(nil)

// NewSSMTokenStore returns a store backed by the named SecureString parameter.
func NewSSMTokenStore(client SSMAPI, name string) *SSMTokenStore {
	return &SSMTokenStore{client: client, name: name}
}

func (s *SSMTokenStore) Get(ctx context.Context) (string, error) {
	out, err := s.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(s.name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		var notFound *ssmtypes.ParameterNotFound
		if errors.As(err, &notFound) {
			return "", fmt.Errorf("%w (parameter %s)", ErrTokenNotFound, s.name)
		}
		return "", fmt.Errorf("config: read %s: %w", s.name, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", fmt.Errorf("%w (parameter %s is empty)", ErrTokenNotFound, s.name)
	}
	return *out.Parameter.Value, nil
}

// Put overwrites the parameter.
//
// PutParameter is idempotent, so the caller may safely retry: that matters because a failed
// write here means Spotify has likely already invalidated the previous token and the new
// one exists only in memory.
func (s *SSMTokenStore) Put(ctx context.Context, token string) error {
	if token == "" {
		return errors.New("config: refusing to store an empty refresh token")
	}
	_, err := s.client.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(s.name),
		Value:     aws.String(token),
		Type:      ssmtypes.ParameterTypeSecureString,
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("config: write %s: %w", s.name, err)
	}
	return nil
}

// FileTokenStore keeps the refresh token in a local JSON file with 0600 permissions.
//
// It exists so the capture pipeline can be run and verified before an AWS account has been
// chosen, and so day-to-day development does not touch production state. It is NOT for
// production: the file is unencrypted at rest, which is why it is opt-in via an explicit
// path rather than a fallback.
type FileTokenStore struct {
	path string
	mu   sync.Mutex
}

var _ spotify.RefreshTokenStore = (*FileTokenStore)(nil)

// NewFileTokenStore returns a store backed by path.
func NewFileTokenStore(path string) *FileTokenStore {
	return &FileTokenStore{path: path}
}

// Path reports the backing file, for operator-facing messages.
func (s *FileTokenStore) Path() string { return s.path }

type tokenFile struct {
	RefreshToken string `json:"refresh_token"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	Note         string `json:"_note,omitempty"`
}

func (s *FileTokenStore) Get(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w (file %s)", ErrTokenNotFound, s.path)
		}
		return "", fmt.Errorf("config: read %s: %w", s.path, err)
	}
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		// Tolerate a file containing just the bare token, which is what someone hand-
		// editing it is most likely to produce.
		if bare := strings.TrimSpace(string(b)); bare != "" && !strings.HasPrefix(bare, "{") {
			return bare, nil
		}
		return "", fmt.Errorf("config: parse %s: %w", s.path, err)
	}
	if tf.RefreshToken == "" {
		return "", fmt.Errorf("%w (file %s has no refresh_token)", ErrTokenNotFound, s.path)
	}
	return tf.RefreshToken, nil
}

// Put writes the token atomically: a temp file in the same directory, then a rename. A
// half-written token file would be indistinguishable from a revoked one.
func (s *FileTokenStore) Put(ctx context.Context, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if token == "" {
		return errors.New("config: refusing to store an empty refresh token")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create %s: %w", dir, err)
	}

	body, err := json.MarshalIndent(tokenFile{
		RefreshToken: token,
		Note:         "Spotify refresh token. Local development only -- unencrypted at rest.",
	}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return fmt.Errorf("config: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("config: rename into %s: %w", s.path, err)
	}
	return nil
}
