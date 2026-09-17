package virtuallibrary

import (
	"context"
	"errors"
	"testing"
)

type fakeRetirer struct {
	stops   []int
	clears  int
	deletes []int
	stopErr error
	delErr  error
}

func (f *fakeRetirer) Stop(id int) error {
	f.stops = append(f.stops, id)
	return f.stopErr
}

func (f *fakeRetirer) ClearCaches() { f.clears++ }

func (f *fakeRetirer) DeleteInstallation(_ context.Context, id int) error {
	f.deletes = append(f.deletes, id)
	return f.delErr
}

func TestRetireRefusesWithoutMigration(t *testing.T) {
	ctx := context.Background()
	st := newFakeSettings() // no marker
	inst := &fakeInstallations{list: []*PluginInstallation{{ID: 5, PluginID: VirtualLibraryPluginID}}}
	r := &fakeRetirer{}
	if _, err := RetirePlugin(ctx, inst, r, st, testLogger()); err == nil {
		t.Fatalf("expected refusal without migration marker")
	}
	if len(r.deletes) != 0 {
		t.Fatalf("installation deleted without verified migration")
	}
}

func TestRetireStopsClearsDeletes(t *testing.T) {
	ctx := context.Background()
	st := newFakeSettings()
	st.values[migratedMarkerKey] = "true"
	st.values["virtual_library.migrated_installation_5"] = "true"
	inst := &fakeInstallations{list: []*PluginInstallation{{ID: 5, PluginID: VirtualLibraryPluginID}}}
	r := &fakeRetirer{}
	res, err := RetirePlugin(ctx, inst, r, st, testLogger())
	if err != nil {
		t.Fatalf("RetirePlugin: %v", err)
	}
	if !res.Retired || res.InstallationID != 5 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(r.stops) != 1 || r.stops[0] != 5 {
		t.Fatalf("stop not called first: %v", r.stops)
	}
	if r.clears != 1 {
		t.Fatalf("caches not cleared: %d", r.clears)
	}
	if len(r.deletes) != 1 || r.deletes[0] != 5 {
		t.Fatalf("delete not called: %v", r.deletes)
	}
}

func TestRetireNoInstallationNoop(t *testing.T) {
	ctx := context.Background()
	st := newFakeSettings()
	st.values[migratedMarkerKey] = "true"
	r := &fakeRetirer{}
	res, err := RetirePlugin(ctx, &fakeInstallations{}, r, st, testLogger())
	if err != nil {
		t.Fatalf("RetirePlugin: %v", err)
	}
	if res.Retired {
		t.Fatalf("nothing installed but retired: %+v", res)
	}
}

func TestRetireStopFailureStillDeletes(t *testing.T) {
	ctx := context.Background()
	st := newFakeSettings()
	st.values[migratedMarkerKey] = "true"
	st.values["virtual_library.migrated_installation_6"] = "true"
	inst := &fakeInstallations{list: []*PluginInstallation{{ID: 6, PluginID: VirtualLibraryPluginID}}}
	r := &fakeRetirer{stopErr: errors.New("process gone")}
	res, err := RetirePlugin(ctx, inst, r, st, testLogger())
	if err != nil {
		t.Fatalf("stop failure must not block retirement: %v", err)
	}
	if !res.Retired || len(r.deletes) != 1 {
		t.Fatalf("unexpected result: %+v deletes=%v", res, r.deletes)
	}
}

func TestRetireDeleteFailure(t *testing.T) {
	ctx := context.Background()
	st := newFakeSettings()
	st.values[migratedMarkerKey] = "true"
	st.values["virtual_library.migrated_installation_7"] = "true"
	inst := &fakeInstallations{list: []*PluginInstallation{{ID: 7, PluginID: VirtualLibraryPluginID}}}
	r := &fakeRetirer{delErr: errors.New("fk violation")}
	if _, err := RetirePlugin(ctx, inst, r, st, testLogger()); err == nil {
		t.Fatalf("expected delete error")
	}
}
