package artworkstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

type testSettings struct {
	values map[string]string
	writes int
}

type flakySettings struct {
	testSettings
	fail bool
}

func (s *flakySettings) SetIfAbsent(ctx context.Context, key, value string) (bool, error) {
	if s.fail {
		s.fail = false
		return false, context.Canceled
	}
	return s.testSettings.SetIfAbsent(ctx, key, value)
}

func (s *testSettings) Get(_ context.Context, key string) (string, error) { return s.values[key], nil }
func (s *testSettings) Set(_ context.Context, key, value string) error {
	s.writes++
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[key] = value
	return nil
}
func (s *testSettings) SetIfAbsent(_ context.Context, key, value string) (bool, error) {
	s.writes++
	if s.values[key] != "" {
		return false, nil
	}
	s.values[key] = value
	return true, nil
}

func TestOpenLocalRecordsBackendOnFirstPut(t *testing.T) {
	settings := &testSettings{values: map[string]string{}}
	store, backend, err := Open(context.Background(), Options{Backend: "auto", LocalPath: filepath.Join(t.TempDir(), "artwork"), Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	if backend != BackendLocal {
		t.Fatalf("backend=%q", backend)
	}
	if settings.writes != 0 {
		t.Fatal("recorded before write")
	}
	if err = store.Put(context.Background(), "a.webp", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if settings.values[IdentitySettingKey] != store.Identity() || settings.writes != 1 {
		t.Fatalf("settings=%#v writes=%d", settings.values, settings.writes)
	}
}
func TestOpenRejectsRecordedStorageMismatch(t *testing.T) {
	root := t.TempDir()
	current, err := NewFilesystem(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, recorded := range map[string]string{
		"other backend": BackendS3 + "|https://s3.example|artwork|",
		"other root":    BackendLocal + "|" + filepath.Join(root, "elsewhere"),
	} {
		settings := &testSettings{values: map[string]string{IdentitySettingKey: recorded}}
		if _, _, err := Open(context.Background(), Options{Backend: BackendLocal, LocalPath: root, Settings: settings}); err == nil {
			t.Fatalf("%s: mismatch accepted", name)
		}
	}
	settings := &testSettings{values: map[string]string{IdentitySettingKey: current.Identity()}}
	if _, _, err := Open(context.Background(), Options{Backend: BackendLocal, LocalPath: root, Settings: settings}); err != nil {
		t.Fatalf("same root rejected: %v", err)
	}
}

func TestOpenRetriesBackendRecordingAfterSettingsFailure(t *testing.T) {
	settings := &flakySettings{testSettings: testSettings{values: map[string]string{}}, fail: true}
	store, _, err := Open(context.Background(), Options{Backend: BackendLocal, LocalPath: filepath.Join(t.TempDir(), "artwork"), Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "a.webp", []byte("x")); err == nil {
		t.Fatal("write hid backend recording failure")
	}
	if err := store.Put(context.Background(), "a.webp", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if settings.values[IdentitySettingKey] != store.Identity() {
		t.Fatalf("settings = %#v", settings.values)
	}
}

// A release before the identity row lowercased the whole S3 endpoint. The
// migration carries that fingerprint over verbatim, so a mixed-case endpoint
// path must still open and the row is rewritten in the exact form.
func TestOpenUpgradesLegacyLowercasedS3Identity(t *testing.T) {
	const current = BackendS3 + "|https://gateway.example/TenantA|artwork|silo"
	settings := &testSettings{values: map[string]string{IdentitySettingKey: strings.ToLower(current)}}
	store := &identityStore{Store: &Filesystem{root: "/unused"}, identity: current}
	if _, _, err := openRecorded(context.Background(), store, settings); err != nil {
		t.Fatalf("legacy fingerprint rejected: %v", err)
	}
	if settings.values[IdentitySettingKey] != current {
		t.Fatalf("identity not upgraded: %q", settings.values[IdentitySettingKey])
	}
	// A genuinely different path is still a move.
	other := &identityStore{Store: store.Store, identity: BackendS3 + "|https://gateway.example/TenantB|artwork|silo"}
	if _, _, err := openRecorded(context.Background(), other, settings); err == nil {
		t.Fatal("different tenant accepted")
	}
	// The legacy fingerprint kept key-prefix case, so a case-only prefix
	// change is a real move and must not ride the endpoint upgrade.
	prefixCase := &identityStore{Store: store.Store, identity: BackendS3 + "|https://gateway.example/TenantA|artwork|Silo"}
	prefixSettings := &testSettings{values: map[string]string{IdentitySettingKey: BackendS3 + "|https://gateway.example/tenanta|artwork|silo"}}
	if _, _, err := openRecorded(context.Background(), prefixCase, prefixSettings); err == nil {
		t.Fatal("key-prefix case change accepted")
	}
	// A recorded bucket in mixed case never came from the legacy writer.
	bucketCase := &testSettings{values: map[string]string{IdentitySettingKey: BackendS3 + "|https://gateway.example/tenanta|Artwork|silo"}}
	if _, _, err := openRecorded(context.Background(), store, bucketCase); err == nil {
		t.Fatal("mixed-case recorded bucket accepted")
	}
	// Local identities never had a legacy form; case differences are moves.
	local := &identityStore{Store: store.Store, identity: BackendLocal + "|/srv/Art"}
	localSettings := &testSettings{values: map[string]string{IdentitySettingKey: BackendLocal + "|/srv/art"}}
	if _, _, err := openRecorded(context.Background(), local, localSettings); err == nil {
		t.Fatal("local case difference accepted")
	}
}

type identityStore struct {
	Store
	identity string
}

func (s *identityStore) Identity() string { return s.identity }
