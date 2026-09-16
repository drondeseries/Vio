package artworkstore

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Silo-Server/silo-server/internal/s3client"
)

type SettingsStore interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string) error
	SetIfAbsent(context.Context, string, string) (bool, error)
}

type Options struct {
	Backend   string
	LocalPath string
	S3        *s3client.Client
	Settings  SettingsStore
}

func Open(ctx context.Context, opts Options) (Store, string, error) {
	backend := strings.ToLower(strings.TrimSpace(opts.Backend))
	if backend == "" || backend == "auto" {
		if opts.S3 != nil {
			backend = BackendS3
		} else {
			backend = BackendLocal
		}
	}
	var store Store
	var err error
	switch backend {
	case BackendLocal:
		store, err = NewFilesystem(opts.LocalPath)
	case BackendS3:
		if opts.S3 == nil {
			return nil, "", fmt.Errorf("artwork storage backend s3 is configured but no S3 client is available")
		}
		store = NewS3(opts.S3)
	default:
		return nil, "", fmt.Errorf("unknown artwork storage backend %q", opts.Backend)
	}
	if err != nil {
		return nil, "", err
	}
	// Availability is checked by readiness through Probe, allowing outage recovery.
	if opts.Settings == nil {
		return store, backend, nil
	}
	recorded, _, err := openRecorded(ctx, store, opts.Settings)
	if err != nil {
		return nil, "", err
	}
	return recorded, backend, nil
}

// openRecorded binds store to the identity recorded in settings: it refuses a
// store the catalog does not belong to and wraps the store so its first write
// records the identity. The middle return is the recorded identity after any
// legacy upgrade, for tests.
func openRecorded(ctx context.Context, store Store, settings SettingsStore) (Store, string, error) {
	// The catalog's keys belong to exactly one storage location. Opening a
	// different one, whether another backend or another bucket or root, would
	// serve a catalog whose objects live elsewhere.
	active, err := settings.Get(ctx, IdentitySettingKey)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", IdentitySettingKey, err)
	}
	if active != "" && active != store.Identity() {
		if !legacyIdentityMatches(active, store.Identity()) {
			return nil, "", fmt.Errorf("artwork storage is recorded as %q but configured as %q; copy the artwork tree to the new storage, then delete the %s row", active, store.Identity(), IdentitySettingKey)
		}
		// The row was translated from a release that lowercased the whole
		// endpoint. It names this store; rewrite it in the exact form so the
		// next start compares equal without this detour.
		if err := settings.Set(ctx, IdentitySettingKey, store.Identity()); err != nil {
			return nil, "", fmt.Errorf("upgrade %s: %w", IdentitySettingKey, err)
		}
		active = store.Identity()
	}
	wrapped := &recordingStore{Store: store, settings: settings}
	if direct, ok := store.(DirectURLer); ok {
		return &recordingDirectStore{recordingStore: wrapped, DirectURLer: direct}, active, nil
	}
	return wrapped, active, nil
}

// legacyIdentityMatches reports whether recorded is the pre-1.0 S3 fingerprint
// of current. Releases before the single identity row lowercased the entire
// endpoint, path included, and the migration carries that value over as
// "s3|<fingerprint>". Only the endpoint is compared that way: the bucket was
// always lowercased, and the key prefix always kept its case, so both must
// match exactly. A recorded lowercase endpoint path against a mixed-case
// configured one is ambiguous between "same store, older normalization" and
// "moved to a sibling tenant"; the upgrade path takes the first reading,
// because a move that differs only by path case while the bucket and prefix
// stay put is not a deployment anyone performs by accident.
func legacyIdentityMatches(recorded, current string) bool {
	recordedParts := strings.Split(recorded, "|")
	currentParts := strings.Split(current, "|")
	if len(recordedParts) != 4 || len(currentParts) != 4 || currentParts[0] != BackendS3 || recordedParts[0] != BackendS3 {
		return false
	}
	return recordedParts[1] == strings.ToLower(currentParts[1]) &&
		recordedParts[2] == currentParts[2] &&
		recordedParts[3] == currentParts[3]
}

type recordingStore struct {
	Store
	settings SettingsStore
	mu       sync.Mutex
	recorded bool
}

type recordingDirectStore struct {
	*recordingStore
	DirectURLer
}

func (s *recordingDirectStore) ObjectAvailable(ctx context.Context, key string) (bool, error) {
	checker, ok := s.Store.(interface {
		ObjectAvailable(context.Context, string) (bool, error)
	})
	if !ok {
		return false, fmt.Errorf("artwork backend does not support external availability checks")
	}
	return checker.ObjectAvailable(ctx, key)
}

func (s *recordingStore) Put(ctx context.Context, key string, data []byte) error {
	if err := s.Store.Put(ctx, key, data); err != nil {
		return err
	}
	return s.recordBackend(ctx)
}

func (s *recordingStore) recordBackend(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recorded {
		return nil
	}
	identity := s.Identity()
	inserted, err := s.settings.SetIfAbsent(ctx, IdentitySettingKey, identity)
	if err != nil {
		return fmt.Errorf("record artwork storage: %w", err)
	}
	if !inserted {
		active, err := s.settings.Get(ctx, IdentitySettingKey)
		if err != nil {
			return fmt.Errorf("verify recorded artwork storage: %w", err)
		}
		if active != identity {
			return fmt.Errorf("artwork storage changed concurrently: recorded %q, writing %q", active, identity)
		}
	}
	s.recorded = true
	return nil
}
