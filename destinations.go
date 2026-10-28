package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
	"github.com/tellmeY18/containers_anywhere/internal/hostapi"
)

const (
	destinationsFile = "destinations.json"
	maxDestinations  = 16
	destinationCache = 30 * time.Second
)

// destinationRecord is what the launcher remembers about an enrolled destination.
// The path stays on the host: it is never published to the cluster.
type destinationRecord struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Device string `json:"device,omitempty"` // the disk it was enrolled on
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func destinationSlug(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "destination"
	}
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

func loadDestinations(state string) ([]destinationRecord, error) {
	b, err := os.ReadFile(filepath.Join(state, destinationsFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []destinationRecord
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s is unreadable: %w", destinationsFile, err)
	}
	return list, nil
}

func saveDestinations(state string, list []destinationRecord) error {
	b, _ := json.MarshalIndent(list, "", "  ")
	return atomicWrite(filepath.Join(state, destinationsFile), append(b, '\n'), 0600)
}

// probeWritable proves the folder accepts a new file, then removes it.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".containers-anywhere-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.WriteString("ok")
	cerr := f.Close()
	rerr := os.Remove(name)
	return errors.Join(werr, cerr, rerr)
}

// addDestination enrols a local folder or mounted disk. Only the launcher (the
// panel or the command line on this computer) calls it.
func addDestination(state, name, path string) (destinationRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 || strings.ContainsAny(name, "\x00\r\n") {
		return destinationRecord{}, errors.New("give the destination a name of up to 80 characters")
	}
	if path == "" {
		return destinationRecord{}, errors.New("a folder is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return destinationRecord{}, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return destinationRecord{}, fmt.Errorf("the folder is not available: %w", err)
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		return destinationRecord{}, errors.New("the destination must be an existing folder")
	}
	stateReal, err := filepath.EvalSymlinks(state)
	if err != nil {
		stateReal = state
	}
	if within(stateReal, real) || within(real, stateReal) {
		return destinationRecord{}, errors.New("choose a folder outside the cluster state directory (and not one that contains it)")
	}
	if err := probeWritable(real); err != nil {
		return destinationRecord{}, fmt.Errorf("cannot write to that folder: %w", err)
	}
	if _, err := diskspace.Of(real); err != nil {
		return destinationRecord{}, fmt.Errorf("cannot read free space: %w", err)
	}
	list, err := loadDestinations(state)
	if err != nil {
		return destinationRecord{}, err
	}
	if len(list) >= maxDestinations {
		return destinationRecord{}, errors.New("too many destinations; remove one first")
	}
	used := map[string]bool{}
	for _, d := range list {
		used[d.ID] = true
		if d.Path == real {
			return destinationRecord{}, errors.New("that folder is already a destination")
		}
	}
	id := destinationSlug(name)
	for n := 2; used[id]; n++ {
		id = fmt.Sprintf("%s-%d", destinationSlug(name), n)
	}
	dev, _ := deviceID(real)
	rec := destinationRecord{ID: id, Name: name, Path: real, Device: dev}
	list = append(list, rec)
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return rec, saveDestinations(state, list)
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// removeDestination forgets the destination. It never touches what is in the folder.
func removeDestination(state, id string) error {
	list, err := loadDestinations(state)
	if err != nil {
		return err
	}
	out := list[:0]
	found := false
	for _, d := range list {
		if d.ID == id {
			found = true
			continue
		}
		out = append(out, d)
	}
	if !found {
		return errors.New("unknown destination")
	}
	return saveDestinations(state, out)
}

// checkDestination reports the truth about one destination now.
func checkDestination(state string, d destinationRecord, now time.Time) hostapi.Destination {
	out := hostapi.Destination{ID: d.ID, Name: d.Name, Kind: "folder", CheckedAt: now.UTC().Format(time.RFC3339)}
	fail := func(msg string) hostapi.Destination { out.LastError = msg; return out }
	info, err := os.Stat(d.Path)
	if err != nil || !info.IsDir() {
		return fail("The folder is not there. If it is on a removable disk, connect the disk.")
	}
	dev, err := deviceID(d.Path)
	if err != nil {
		return fail("Cannot identify the disk: " + err.Error())
	}
	if d.Device != "" && dev != d.Device {
		return fail("A different disk is mounted at this location than the one that was enrolled.")
	}
	if err := probeWritable(d.Path); err != nil {
		return fail("The folder is not writable: " + trimTo(err.Error(), 160))
	}
	u, err := diskspace.Of(d.Path)
	if err != nil {
		return fail("Cannot read free space: " + trimTo(err.Error(), 160))
	}
	out.Available, out.FreeBytes, out.TotalBytes = true, int64(u.Free), int64(u.Total)
	if sd, err := deviceID(state); err == nil && sd == dev {
		out.SameDeviceAsData = true
	}
	return out
}

// destinationChecker caches the checks so a removable disk is not written to on
// every five-second status push.
type destinationChecker struct {
	mu    sync.Mutex
	at    time.Time
	known []hostapi.Destination
}

func (c *destinationChecker) facts(state string) []hostapi.Destination {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < destinationCache {
		return c.known
	}
	list, err := loadDestinations(state)
	if err != nil {
		return c.known
	}
	now := time.Now()
	out := make([]hostapi.Destination, 0, len(list))
	for _, d := range list {
		out = append(out, checkDestination(state, d, now))
	}
	c.known, c.at = out, now
	return out
}

func (c *destinationChecker) forget() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

type destinationView struct {
	hostapi.Destination
	Path string `json:"path"`
}

// describeDestinations is for the launcher panel and command line on this computer,
// which may show the path. The cluster never receives it.
func describeDestinations(state string) []destinationView {
	list, _ := loadDestinations(state)
	now := time.Now()
	out := make([]destinationView, 0, len(list))
	for _, d := range list {
		out = append(out, destinationView{Destination: checkDestination(state, d, now), Path: d.Path})
	}
	return out
}

func runDestinations(state, operation, id, name, path string) error {
	switch operation {
	case "list", "status", "":
		b, _ := json.MarshalIndent(describeDestinations(state), "", "  ")
		fmt.Println(string(b))
		return nil
	case "add":
		rec, err := addDestination(state, name, path)
		if err == nil {
			fmt.Printf("Added destination %s (%s)\n", rec.ID, rec.Name)
		}
		return err
	case "remove":
		if err := removeDestination(state, id); err != nil {
			return err
		}
		fmt.Println("Removed. Files in the folder were not touched.")
		return nil
	}
	return errors.New("destinations operation must be list, add or remove")
}
