package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/settingscontract"
	"github.com/Silo-Server/silo-server/internal/settingskeys"
	"github.com/Silo-Server/silo-server/internal/userstore"
	"github.com/Silo-Server/silo-server/internal/userstore/pgstore"
)

// versionSortFixture is one seeded account + profile and the real settings
// values handler backed by Postgres.
type versionSortFixture struct {
	handler *SettingValuesHandler
	store   userstore.UserStore
	userID  int
	profile string
}

// newVersionSortFixture connects to the disposable database provisioned by the
// package TestMain (disposable_db_test.go). Without SILO_TEST_DATABASE_URL the
// test skips, matching the other Postgres-backed handler suites.
func newVersionSortFixture(t *testing.T) versionSortFixture {
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
		fmt.Sprintf("version-sort-%d", suffix),
		fmt.Sprintf("version-sort-%d@invalid.test", suffix)).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	store, err := provider.ForUser(ctx, userID)
	if err != nil {
		t.Fatalf("user store: %v", err)
	}
	profile := fmt.Sprintf("version-sort-%d", suffix)
	if err := store.CreateProfile(ctx, userstore.Profile{ID: profile, Name: "Version sort"}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	return versionSortFixture{
		handler: NewSettingValuesHandler(provider, contract),
		store:   store,
		userID:  userID,
		profile: profile,
	}
}

func (f versionSortFixture) request(method, target string, body []byte) *http.Request {
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, bytes.NewReader(body))
	}
	req.Header.Set(deviceIDHeader, "device-1")
	req.Header.Set(clientFamilyHeader, "web")
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("key", settingskeys.PlaybackVersionSort)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx)
	ctx = apimw.SetClaims(ctx, &auth.Claims{UserID: f.userID, Role: "user"})
	ctx = apimw.SetProfileID(ctx, f.profile)
	return req.WithContext(ctx)
}

func (f versionSortFixture) put(value string) *httptest.ResponseRecorder {
	req := f.request(http.MethodPut,
		"/settings/values/"+settingskeys.PlaybackVersionSort+"?scope=profile",
		[]byte(`{"value":`+value+`}`))
	rec := httptest.NewRecorder()
	f.handler.HandleSetValue(rec, req)
	return rec
}

func (f versionSortFixture) effective(t *testing.T) effectiveSettingValueResponse {
	t.Helper()
	req := f.request(http.MethodGet,
		"/settings/values/effective?keys="+settingskeys.PlaybackVersionSort, nil)
	rec := httptest.NewRecorder()
	f.handler.HandleGetEffective(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("effective = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Settings []effectiveSettingValueResponse `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode effective response: %v", err)
	}
	if len(body.Settings) != 1 {
		t.Fatalf("effective returned %d settings, want 1", len(body.Settings))
	}
	return body.Settings[0]
}

func (f versionSortFixture) deleteValue() *httptest.ResponseRecorder {
	req := f.request(http.MethodDelete,
		"/settings/values/"+settingskeys.PlaybackVersionSort+"?scope=profile", nil)
	rec := httptest.NewRecorder()
	f.handler.HandleDeleteValue(rec, req)
	return rec
}

// sameSortValue compares two version-sort values semantically. Postgres jsonb
// re-serializes arrays with its own whitespace and key order, so a byte compare
// of the stored row would fail on formatting alone.
func sameSortValue(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode %s: %v", want, err)
	}
	return reflect.DeepEqual(gotValue, wantValue)
}

// TestVersionSortSettingRoundTripAtProfileScope is the end-to-end contract
// exercise: a profile-scope set is visible through effective resolution,
// an empty list is the explicit "no override" form, and DELETE removes the row
// so resolution falls back to the contract default.
func TestVersionSortSettingRoundTripAtProfileScope(t *testing.T) {
	f := newVersionSortFixture(t)

	order := `[{"attribute":"resolution","direction":"desc"},{"attribute":"bitrate","direction":"desc"}]`
	if rec := f.put(order); rec.Code != http.StatusOK {
		t.Fatalf("set version sort = %d: %s", rec.Code, rec.Body.String())
	}
	if got := f.effective(t); !sameSortValue(t, got.Value, order) || got.Source != "profile" {
		t.Fatalf("effective = %s from %s, want the stored order from profile", got.Value, got.Source)
	}

	// The stored row is an explicit profile value, not a default.
	stored, err := f.store.GetSettingValue(context.Background(), userstore.SettingIdentity{
		Key: settingskeys.PlaybackVersionSort, Scope: settingscontract.ScopeProfile, ProfileID: f.profile,
	})
	if err != nil || stored == nil {
		t.Fatalf("stored value = %v, err = %v", stored, err)
	}
	if !sameSortValue(t, stored.Value, order) {
		t.Fatalf("stored value = %s, want %s", stored.Value, order)
	}

	// An empty list is the explicit "no override" form: it stores, and the
	// version list treats it the same as absent.
	if rec := f.put(`[]`); rec.Code != http.StatusOK {
		t.Fatalf("clear with empty list = %d: %s", rec.Code, rec.Body.String())
	}
	if got := f.effective(t); !sameSortValue(t, got.Value, `[]`) || got.Source != "profile" {
		t.Fatalf("effective after empty list = %s from %s, want [] from profile", got.Value, got.Source)
	}

	// DELETE removes the row entirely; resolution falls back to the default and
	// the empty stored value no longer wins.
	if rec := f.put(order); rec.Code != http.StatusOK {
		t.Fatalf("re-set version sort = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := f.deleteValue(); rec.Code != http.StatusNoContent {
		t.Fatalf("delete version sort = %d: %s", rec.Code, rec.Body.String())
	}
	if stored, err := f.store.GetSettingValue(context.Background(), userstore.SettingIdentity{
		Key: settingskeys.PlaybackVersionSort, Scope: settingscontract.ScopeProfile, ProfileID: f.profile,
	}); err != nil || stored != nil {
		t.Fatalf("value survived delete: %v, err = %v", stored, err)
	}
	got := f.effective(t)
	if got.Source != "default" || !sameSortValue(t, got.Value, "null") {
		t.Fatalf("effective after delete = %s from %s, want null from default", got.Value, got.Source)
	}
}

// TestVersionSortSettingRejectsInvalidValuesAtProfileScope proves the endpoint
// rejects the shapes the manifest forbids before anything is stored, and that a
// rejected write leaves no row behind.
func TestVersionSortSettingRejectsInvalidValuesAtProfileScope(t *testing.T) {
	f := newVersionSortFixture(t)

	seventeen := "[" + strings.TrimSuffix(strings.Repeat(`{"attribute":"size","direction":"desc"},`, 17), ",") + "]"
	for name, value := range map[string]string{
		"unknown attribute":     `[{"attribute":"filesize","direction":"desc"}]`,
		"server-only attribute": `[{"attribute":"source","direction":"asc"}]`,
		"unknown direction":     `[{"attribute":"size","direction":"sideways"}]`,
		"missing direction":     `[{"attribute":"size"}]`,
		"seventeen entries":     seventeen,
		"object not array":      `{"attribute":"size","direction":"desc"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.put(value)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("invalid value %s = %d: %s", value, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "invalid_value") {
				t.Fatalf("rejection did not carry invalid_value: %s", rec.Body.String())
			}
			stored, err := f.store.GetSettingValue(context.Background(), userstore.SettingIdentity{
				Key: settingskeys.PlaybackVersionSort, Scope: settingscontract.ScopeProfile, ProfileID: f.profile,
			})
			if err != nil {
				t.Fatalf("read after rejected write: %v", err)
			}
			if stored != nil {
				t.Fatalf("rejected write stored %s", stored.Value)
			}
		})
	}
}
