package recovery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/atomicfile"
	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

func New(cfg Config) (*Engine, error) {
	if cfg.Root == "" || cfg.VarLib == "" || cfg.PoolDir == "" || cfg.FS == nil || cfg.Datastore == nil || cfg.Copy == nil || cfg.K3s == "" || cfg.DataFormat == "" || cfg.Appliance == "" || cfg.Architecture == "" {
		return nil, errors.New("incomplete recovery configuration")
	}
	for _, p := range []string{cfg.Root, cfg.VarLib, cfg.PoolDir} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, errors.New("recovery paths must be clean absolute paths")
		}
	}
	if within(cfg.Root, cfg.VarLib) || within(cfg.VarLib, cfg.Root) || !within(cfg.PoolDir, cfg.VarLib) || cfg.PoolDir == cfg.VarLib {
		return nil, errors.New("recovery catalog must be outside /var/lib; pool must be inside it")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.DiskSpace == nil {
		cfg.DiskSpace = diskspace.Of
	}
	for _, p := range []string{cfg.Root, filepath.Join(cfg.Root, "operations"), filepath.Join(cfg.Root, "points"), filepath.Join(cfg.Root, "staging"), filepath.Join(cfg.Root, "populations"), filepath.Join(cfg.Root, "volume-claims")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return nil, err
		}
		st, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("recovery directory is not a real directory")
		}
	}
	if err := syncDir(filepath.Dir(cfg.Root)); err != nil {
		return nil, err
	}
	if err := syncDir(cfg.Root); err != nil {
		return nil, err
	}
	return &Engine{cfg: cfg}, nil
}

func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func sum(b []byte) string   { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func key(uid string) string { return "op-" + sum([]byte(uid)) }

// PointIDForUID is the deterministic, opaque catalog identity for a capture or
// workload registration UID. Adapters use it to deduplicate export receipts.
func PointIDForUID(uid string) string { return key(uid) }
func (e *Engine) operationPath(uid string) string {
	return filepath.Join(e.cfg.Root, "operations", key(uid)+".json")
}
func (e *Engine) pointPath(id string) string { return filepath.Join(e.cfg.Root, "points", id) }
func syncDir(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// Envelopes detect truncation and accidental modification, in addition to strict
// decoding. Authentication of an off-device archive belongs to the host's age layer.
type envelope struct {
	SHA256 string          `json:"sha256"`
	Data   json.RawMessage `json:"data"`
}

func writeRecord(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data, err := json.Marshal(envelope{sum(b), b})
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("recovery record too large")
	}
	return atomicfile.WritePrivate(path, data)
}
func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func readRecord(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if err != nil {
		return err
	}
	if len(b) > 4<<20 {
		return errors.New("record too large")
	}
	var env envelope
	if err := decode(b, &env); err != nil {
		return err
	}
	if sum(env.Data) != env.SHA256 {
		return errors.New("recovery record checksum mismatch")
	}
	return decode(env.Data, v)
}
func (e *Engine) save(op *Operation) error {
	op.UpdatedAt = e.cfg.Now().UTC()
	return writeRecord(e.operationPath(op.Request.UID), op)
}
func active(op Operation) bool { return op.Phase != "completed" && op.Phase != "failed" }
func (e *Engine) requireIdle() error {
	ops, err := e.operations()
	if err != nil {
		return err
	}
	for _, op := range ops {
		if active(op) {
			return ErrBusy
		}
	}
	return e.requireNoPopulation("")
}
func (e *Engine) operations() ([]Operation, error) {
	return readOperations(e.cfg.Root)
}

// readOperations performs no initialization, locking or writes. Boot preflight
// uses the same checksum and contract checks as the serving/cold engine.
func readOperations(root string) ([]Operation, error) {
	dir := filepath.Join(root, "operations")
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, errors.New("recovery operations path is not a real directory")
	}
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []Operation{}
	for _, ent := range es {
		if filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("recovery operation is not a regular file")
		}
		var op Operation
		if err := readRecord(filepath.Join(dir, ent.Name()), &op); err != nil {
			return nil, fmt.Errorf("operation %s: %w", ent.Name(), err)
		}
		if op.Version != Version || ent.Name() != key(op.Request.UID)+".json" {
			return nil, errors.New("invalid operation identity/version")
		}
		if !identifier.MatchString(op.Request.UID) || op.Request.ID != op.Request.UID || (op.Request.Action != "capture" && op.Request.Action != "restore") || (op.Request.Scope != "system" && op.Request.Scope != "full") || !identifier.MatchString(op.PointID) {
			return nil, errors.New("invalid recovery operation contract")
		}
		switch op.Phase {
		case "restart-needed", "capturing", "restoring", "rolling-back", "completed", "failed":
		default:
			return nil, errors.New("unknown recovery journal phase")
		}
		if op.Request.Action == "capture" && (op.Request.PointID != "" || op.Phase == "restoring" || op.Phase == "rolling-back") {
			return nil, errors.New("invalid capture journal phase or source")
		}
		if op.Request.Action == "restore" && (op.Request.PointID != op.PointID || op.Phase == "capturing") {
			return nil, errors.New("invalid restore journal phase or source")
		}
		if (op.Phase == "restoring" || op.Phase == "rolling-back") && (op.SafetyID != "safety-"+key(op.Request.UID) || op.SafetyID == op.PointID) {
			return nil, errors.New("invalid destructive restore safety identity")
		}
		out = append(out, op)
	}
	return out, nil
}

func (e *Engine) Status(uid string) (Operation, error) {
	unlock, err := e.lock()
	if err != nil {
		return Operation{}, err
	}
	defer unlock()
	var op Operation
	err = readRecord(e.operationPath(uid), &op)
	if err == nil && (op.Version != Version || op.Request.UID != uid) {
		err = errors.New("invalid operation identity")
	}
	return op, err
}
func (e *Engine) Operations() ([]Operation, error) {
	unlock, err := e.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return e.operations()
}
func (e *Engine) Get(id string) (Manifest, error) {
	unlock, err := e.lock()
	if err != nil {
		return Manifest{}, err
	}
	defer unlock()
	return e.get(id)
}
func (e *Engine) get(id string) (Manifest, error) {
	var m Manifest
	if !identifier.MatchString(id) {
		return m, errors.New("invalid point ID")
	}
	if err := readRecord(filepath.Join(e.pointPath(id), "manifest.json"), &m); err != nil {
		return m, err
	}
	if m.ID != id {
		return m, errors.New("point identity mismatch")
	}
	return m, e.validateManifest(m)
}
func (e *Engine) List() ([]Manifest, error) {
	unlock, err := e.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, err := os.ReadDir(filepath.Join(e.cfg.Root, "points"))
	if err != nil {
		return nil, err
	}
	out := []Manifest{}
	for _, p := range entries {
		if !p.IsDir() {
			continue
		}
		m, err := e.get(p.Name())
		if err != nil {
			return nil, fmt.Errorf("point %s: %w", p.Name(), err)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}
