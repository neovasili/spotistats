package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/neovasili/spotistats/internal/model"
)

// DimensionStaleAfter is how old a metadata row may be before the capture job refreshes it
// opportunistically. Names and genres change rarely, so this is about eventual accuracy,
// not correctness.
const DimensionStaleAfter = 30 * 24 * time.Hour

// PutTrack writes a track metadata row, stamping refreshedAt from the store clock.
func (s *Store) PutTrack(ctx context.Context, t model.Track) error {
	return s.putDimension(ctx, "PutTrack", newTrackItem(t, s.now()))
}

// PutArtist writes an artist metadata row. Genres are stored exactly as Spotify sent them;
// normalisation happens at aggregation time so the UI can display the original strings.
func (s *Store) PutArtist(ctx context.Context, a model.Artist) error {
	return s.putDimension(ctx, "PutArtist", newArtistItem(a, s.now()))
}

// PutAlbum writes an album metadata row.
func (s *Store) PutAlbum(ctx context.Context, a model.Album) error {
	return s.putDimension(ctx, "PutAlbum", newAlbumItem(a, s.now()))
}

func (s *Store) putDimension(ctx context.Context, op string, item any) error {
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("store: marshal dimension: %w", err)
	}
	pk := stringAttr(av, AttrPK)
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	})
	return classify(op, pk, SKMeta, err)
}

// GetTrack reads a track metadata row, returning ErrNotFound when absent.
func (s *Store) GetTrack(ctx context.Context, id string) (model.Track, error) {
	var item trackItem
	if err := s.getDimension(ctx, "GetTrack", TrackPK(id), &item); err != nil {
		return model.Track{}, err
	}
	return item.toModel()
}

// GetArtist reads an artist metadata row.
func (s *Store) GetArtist(ctx context.Context, id string) (model.Artist, error) {
	var item artistItem
	if err := s.getDimension(ctx, "GetArtist", ArtistPK(id), &item); err != nil {
		return model.Artist{}, err
	}
	return item.toModel()
}

// GetAlbum reads an album metadata row.
func (s *Store) GetAlbum(ctx context.Context, id string) (model.Album, error) {
	var item albumItem
	if err := s.getDimension(ctx, "GetAlbum", AlbumPK(id), &item); err != nil {
		return model.Album{}, err
	}
	return item.toModel()
}

func (s *Store) getDimension(ctx context.Context, op, pk string, dst any) error {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       key(pk, SKMeta),
	})
	if err != nil {
		return classify(op, pk, SKMeta, err)
	}
	if out.Item == nil {
		return &Error{Op: op, PK: pk, SK: SKMeta, Err: ErrNotFound}
	}
	if err := attributevalue.UnmarshalMap(out.Item, dst); err != nil {
		return fmt.Errorf("store: unmarshal dimension: %w", err)
	}
	return nil
}

// GetTracks reads many track rows in one round trip. IDs with no row are simply absent
// from the result.
func (s *Store) GetTracks(ctx context.Context, ids []string) (map[string]model.Track, error) {
	out := make(map[string]model.Track, len(ids))
	err := s.batchGetDimensions(ctx, "GetTracks", ids, TrackPK, func(raw map[string]ddbtypes.AttributeValue) error {
		var item trackItem
		if err := attributevalue.UnmarshalMap(raw, &item); err != nil {
			return err
		}
		t, err := item.toModel()
		if err != nil {
			return err
		}
		out[t.ID] = t
		return nil
	})
	return out, err
}

// GetArtists reads many artist rows in one round trip. This is the genre-resolution path
// for aggregation.
func (s *Store) GetArtists(ctx context.Context, ids []string) (map[string]model.Artist, error) {
	out := make(map[string]model.Artist, len(ids))
	err := s.batchGetDimensions(ctx, "GetArtists", ids, ArtistPK, func(raw map[string]ddbtypes.AttributeValue) error {
		var item artistItem
		if err := attributevalue.UnmarshalMap(raw, &item); err != nil {
			return err
		}
		a, err := item.toModel()
		if err != nil {
			return err
		}
		out[a.ID] = a
		return nil
	})
	return out, err
}

// GetAlbums reads many album rows in one round trip.
func (s *Store) GetAlbums(ctx context.Context, ids []string) (map[string]model.Album, error) {
	out := make(map[string]model.Album, len(ids))
	err := s.batchGetDimensions(ctx, "GetAlbums", ids, AlbumPK, func(raw map[string]ddbtypes.AttributeValue) error {
		var item albumItem
		if err := attributevalue.UnmarshalMap(raw, &item); err != nil {
			return err
		}
		a, err := item.toModel()
		if err != nil {
			return err
		}
		out[a.ID] = a
		return nil
	})
	return out, err
}

func (s *Store) batchGetDimensions(
	ctx context.Context, op string, ids []string,
	pkFor func(string) string,
	consume func(map[string]ddbtypes.AttributeValue) error,
) error {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil
	}

	for start := 0; start < len(unique); start += maxBatchGetKeys {
		end := min(start+maxBatchGetKeys, len(unique))
		reqKeys := make([]map[string]ddbtypes.AttributeValue, 0, end-start)
		for _, id := range unique[start:end] {
			reqKeys = append(reqKeys, key(pkFor(id), SKMeta))
		}
		pending := map[string]ddbtypes.KeysAndAttributes{s.table: {Keys: reqKeys}}

		for attempt := 0; len(pending) > 0; attempt++ {
			if attempt > maxUnprocessedRetries {
				return &Error{Op: op, Err: fmt.Errorf("%w: unprocessed keys after %d attempts",
					ErrThrottled, attempt)}
			}
			resp, err := s.db.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: pending})
			if err != nil {
				return classify(op, "", "", err)
			}
			for _, raw := range resp.Responses[s.table] {
				if err := consume(raw); err != nil {
					return fmt.Errorf("store: %s: %w", op, err)
				}
			}
			pending = nil
			if ks, ok := resp.UnprocessedKeys[s.table]; ok && len(ks.Keys) > 0 {
				pending = map[string]ddbtypes.KeysAndAttributes{s.table: ks}
			}
		}
	}
	return nil
}

// PutMissing writes a tombstone for an ID Spotify returned null for.
//
// Without it the enrichment pass would re-request the same permanently-unresolvable IDs on
// every single run, forever, burning rate-limit budget that development-mode quota cannot
// spare.
func (s *Store) PutMissing(ctx context.Context, dim model.Dim, id string) error {
	const op = "PutMissing"
	if id == "" {
		return errors.New("store: PutMissing requires an id")
	}
	switch dim {
	case model.DimTrack:
		return s.PutTrack(ctx, model.Track{ID: id, Missing: true})
	case model.DimArtist:
		return s.PutArtist(ctx, model.Artist{ID: id, Missing: true})
	case model.DimAlbum:
		return s.PutAlbum(ctx, model.Album{ID: id, Missing: true})
	default:
		return fmt.Errorf("store: %s: dimension %q has no metadata rows", op, dim)
	}
}

// IsStale reports whether a metadata row is old enough to refresh. A zero refreshedAt (a
// row written before the field existed, or a tombstone) counts as stale.
func (s *Store) IsStale(refreshedAt time.Time) bool {
	if refreshedAt.IsZero() {
		return true
	}
	return s.now().Sub(refreshedAt) > DimensionStaleAfter
}

func stringAttr(av map[string]ddbtypes.AttributeValue, name string) string {
	if v, ok := av[name].(*ddbtypes.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}
