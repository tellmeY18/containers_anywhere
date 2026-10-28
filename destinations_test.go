package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnrollingADestinationChecksItAndKeepsThePathOnTheHost(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	rec, err := addDestination(state, " Backup disk ", folder)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != "backup-disk" || rec.Name != "Backup disk" {
		t.Fatalf("%+v", rec)
	}
	if entries, _ := os.ReadDir(folder); len(entries) != 0 {
		t.Fatal("the writability probe must clean up after itself")
	}
	if _, err := addDestination(state, "Again", folder); err == nil {
		t.Fatal("the same folder must not be enrolled twice")
	}
	other := t.TempDir()
	rec2, err := addDestination(state, "Backup disk", other)
	if err != nil || rec2.ID != "backup-disk-2" {
		t.Fatalf("ids must be unique: %+v %v", rec2, err)
	}
	got := checkDestination(state, rec, time.Now())
	if !got.Available || got.FreeBytes <= 0 || got.Kind != "folder" {
		t.Fatalf("%+v", got)
	}
	if !got.SameDeviceAsData {
		t.Fatal("a folder on the same disk as the state directory must be labelled as such")
	}
	if strings.Contains(got.LastError+got.Name+got.ID, folder) {
		t.Fatal("no host path in what is published")
	}
}

func TestEnrollingRefusesUnsafeFolders(t *testing.T) {
	state := t.TempDir()
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0600)
	inside := filepath.Join(state, "backups")
	os.Mkdir(inside, 0700)
	for name, path := range map[string]string{
		"missing":      filepath.Join(t.TempDir(), "nope"),
		"a file":       file,
		"inside state": inside,
		"the state":    state,
		"parent":       filepath.Dir(state),
		"empty":        "",
	} {
		if _, err := addDestination(state, "x", path); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	if _, err := addDestination(state, "", t.TempDir()); err == nil {
		t.Error("a name is required")
	}
	if _, err := addDestination(state, "a\nb", t.TempDir()); err == nil {
		t.Error("control characters in a name must be refused")
	}
}

func TestAMissingDiskIsUnavailableNotAnError(t *testing.T) {
	state := t.TempDir()
	parent := t.TempDir()
	folder := filepath.Join(parent, "usb")
	os.Mkdir(folder, 0700)
	rec, err := addDestination(state, "USB", folder)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(folder)
	got := checkDestination(state, rec, time.Now())
	if got.Available || got.LastError == "" {
		t.Fatalf("%+v", got)
	}
	rec.Device = "dev:0"
	os.Mkdir(folder, 0700)
	got = checkDestination(state, rec, time.Now())
	if got.Available || !strings.Contains(got.LastError, "different disk") {
		t.Fatalf("a different disk at the same place must be refused: %+v", got)
	}
}

func TestRemovingADestinationNeverTouchesTheFolder(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	rec, _ := addDestination(state, "Keep", folder)
	os.WriteFile(filepath.Join(folder, "backup.age"), []byte("x"), 0600)
	if err := removeDestination(state, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(folder, "backup.age")); err != nil {
		t.Fatal("files in the destination must survive removal")
	}
	if list, _ := loadDestinations(state); len(list) != 0 {
		t.Fatal("not removed")
	}
	if removeDestination(state, "nope") == nil {
		t.Fatal("an unknown destination must be an error")
	}
}
