package vmm

import (
	"errors"
	"testing"
)

func TestPlatformErrors(t *testing.T) {
	errs := []struct {
		name string
		err  error
	}{
		{"ErrSnapshotNotSupported", ErrSnapshotNotSupported},
		{"ErrMigrationNotSupported", ErrMigrationNotSupported},
		{"ErrHotplugNotSupported", ErrHotplugNotSupported},
		{"ErrTAPNotSupported", ErrTAPNotSupported},
	}
	for _, tt := range errs {
		if tt.err == nil {
			t.Errorf("%s is nil", tt.name)
		}
		if tt.err.Error() == "" {
			t.Errorf("%s.Error() is empty", tt.name)
		}
	}
}

func TestPlatformErrorsAreDistinct(t *testing.T) {
	if errors.Is(ErrSnapshotNotSupported, ErrMigrationNotSupported) {
		t.Error("ErrSnapshotNotSupported should not match ErrMigrationNotSupported")
	}
	if errors.Is(ErrTAPNotSupported, ErrHotplugNotSupported) {
		t.Error("ErrTAPNotSupported should not match ErrHotplugNotSupported")
	}
}

func TestPlatformErrorsMatchSelf(t *testing.T) {
	if !errors.Is(ErrSnapshotNotSupported, ErrSnapshotNotSupported) {
		t.Error("ErrSnapshotNotSupported should match itself")
	}
}
