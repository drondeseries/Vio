package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/settingscontract"
	"github.com/Silo-Server/silo-server/internal/settingskeys"
	"github.com/Silo-Server/silo-server/internal/userstore"
	"github.com/Silo-Server/silo-server/internal/userstore/pgstore"
)

// effectiveReadFixture is one seeded account + profile and the real settings
// values handler backed by Postgres.
type effectiveReadFixture struct {
	handler *SettingValuesHandler
	userID  int
	profile string
}

// newEffectiveReadFixture connects to the disposable database provisioned by
// the package TestMain (disposable_db_test.go). Without SILO_TEST_DATABASE_URL
// the test skips, matching the other Postgres-backed handler suites.
func newEffectiveReadFixture(t *testing.T) effectiveReadFixture {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	contract, err := settingscontract.Load()
	if err != nil {
		t.Fatalf("loading contract: %v", err)
	}
	provider := pgstore.NewPostgresProvider(pool)

	var userID int
	suffix := time.Now().UnixNano()
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (username, email, password_hash, role)
		VALUES ($1, $2, '', 'user')
		RETURNING id`,
		fmt.Sprintf("effective-read-%d", suffix),
		fmt.Sprintf("effective-read-%d@invalid.test", suffix)).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	store, err := provider.ForUser(ctx, userID)
	if err != nil {
		t.Fatalf("user store: %v", err)
	}
	profile := fmt.Sprintf("effective-read-%d", suffix)
	if err := store.CreateProfile(ctx, userstore.Profile{ID: profile, Name: "Effective read"}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	return effectiveReadFixture{
		handler: NewSettingValuesHandler(provider, contract),
		userID:  userID,
		profile: profile,
	}
}

func effectiveReadViewsByKey(t *testing.T, views []EffectiveSettingValueView) map[string]EffectiveSettingValueView {
	t.Helper()
	byKey := make(map[string]EffectiveSettingValueView, len(views))
	for _, view := range views {
		byKey[view.Key] = view
	}
	return byKey
}

// TestEffectiveReadResolvesWithoutDeviceHeader pins the live 422 regression: a
// non-admin playback client read the effective settings with a profile but no
// X-Vio-Device-Id header, and the read fail-closed (422 at the API boundary)
// whenever a requested key permitted an exact-device override. playback.
// audio_language is exactly such a key, so the owner's configured default was
// never read and playback fell back to its own default. The read must resolve
// the profile/account/default layers it can and name the winning source.
func TestEffectiveReadResolvesWithoutDeviceHeader(t *testing.T) {
	f := newEffectiveReadFixture(t)
	ctx := context.Background()

	if _, err := f.handler.SetSettingValue(ctx, f.userID, SettingIdentityRequest{
		Key:             settingskeys.PlaybackAudioLanguage,
		Scope:           string(settingscontract.ScopeProfile),
		ActiveProfileID: f.profile,
	}, json.RawMessage(`"en"`)); err != nil {
		t.Fatalf("store audio language: %v", err)
	}

	views, err := f.handler.ResolveEffectiveSettings(ctx, f.userID, EffectiveSettingsQuery{
		Keys:            []string{settingskeys.PlaybackAudioLanguage, settingskeys.PlaybackVersionSort},
		ActiveProfileID: f.profile,
		// Device.DeviceID and DeviceID stay empty: the client sends no
		// X-Vio-Device-Id on this read.
	})
	if err != nil {
		t.Fatalf("effective read without a device header failed: %v", err)
	}
	byKey := effectiveReadViewsByKey(t, views)

	audio, ok := byKey[settingskeys.PlaybackAudioLanguage]
	if !ok {
		t.Fatalf("audio language missing from %#v", views)
	}
	if audio.Source != string(settingscontract.ScopeProfile) {
		t.Fatalf("audio source = %q, want profile", audio.Source)
	}
	var language string
	if err := json.Unmarshal(audio.Value, &language); err != nil || language != "en" {
		t.Fatalf("audio value = %s (%v), want \"en\"", audio.Value, err)
	}

	// The profile-only key reads regardless; it must be present and defaulted
	// rather than omitted because the device header was absent.
	if _, ok := byKey[settingskeys.PlaybackVersionSort]; !ok {
		t.Fatalf("version sort missing from %#v", views)
	}
}

// TestEffectiveReadTreatsEmptyKeysAsOmitted pins the other viewer shape the
// review flagged: a client that serializes an empty key list as `?keys=` sends
// one empty key. That is "no keys named" (v1 filtered it too), not an unknown
// setting, so it resolves every remote definition instead of failing.
func TestEffectiveReadTreatsEmptyKeysAsOmitted(t *testing.T) {
	f := newEffectiveReadFixture(t)

	views, err := f.handler.ResolveEffectiveSettings(context.Background(), f.userID, EffectiveSettingsQuery{
		Keys:            []string{""},
		ActiveProfileID: f.profile,
	})
	if err != nil {
		t.Fatalf("empty keys read failed: %v", err)
	}
	if _, ok := effectiveReadViewsByKey(t, views)[settingskeys.PlaybackAudioLanguage]; !ok {
		t.Fatalf("empty keys resolved %d settings without audio_language", len(views))
	}
}

// TestEffectiveReadStillRejectsUnknownKey keeps the malformed-input contract:
// a genuine unknown key is still refused, with a detail that names it.
func TestEffectiveReadStillRejectsUnknownKey(t *testing.T) {
	f := newEffectiveReadFixture(t)

	_, err := f.handler.ResolveEffectiveSettings(context.Background(), f.userID, EffectiveSettingsQuery{
		Keys:            []string{"no.such.setting"},
		ActiveProfileID: f.profile,
	})
	if err == nil {
		t.Fatal("unknown key was accepted")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != 404 || apiErr.Field != settingFieldKeys {
		t.Fatalf("unknown key error = %#v", err)
	}
	if !strings.Contains(apiErr.Message, "no.such.setting") {
		t.Fatalf("unknown key detail does not name the key: %q", apiErr.Message)
	}
}
