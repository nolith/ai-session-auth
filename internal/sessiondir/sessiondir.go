// Package sessiondir is one session's directory,
// $XDG_STATE_HOME/geas/sessions/<session id>: owner-only, holding the ledger
// and everything the session hands the harness's tools. Every file in it is
// written atomically, so a reader sees a whole file or none, and the markers
// in it say how far each provider got.
package sessiondir

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/nolith/geas/internal/secretfile"
)

// Dir is one session's directory.
type Dir struct {
	id   string
	path string
}

// sessionID is a UTC timestamp and 64 random bits: sessions sort by start
// time in a listing, and two started in the same second still differ.
var sessionID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{16}$`)

// NewID returns a new session ID, such as 20261022T213405Z-1f0c2a9b3e4d5c6a.
func NewID(now time.Time) (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random), nil
}

// Path is the directory a session with this ID has, or would have.
func Path(id string) (string, error) {
	if !sessionID.MatchString(id) {
		return "", errors.New("invalid session ID")
	}
	state, err := secretfile.StateDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "sessions", id), nil
}

// Create makes the session's directory, owner-only, and syncs it into its
// parent so that the ledger written next survives a crash. It refuses a
// session ID already in use.
func Create(id string) (*Dir, error) {
	dir, err := Path(id)
	if err != nil {
		return nil, err
	}
	sessions := filepath.Dir(dir)
	if err = os.MkdirAll(sessions, 0700); err != nil {
		return nil, err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	if err = syncDirectory(sessions); err != nil {
		return nil, err
	}
	return &Dir{id: id, path: dir}, nil
}

// Open opens an existing session's directory by ID, as the reaper does once
// the launcher is gone.
func Open(id string) (*Dir, error) {
	dir, err := Path(id)
	if err != nil {
		return nil, err
	}
	return at(dir)
}

// FromEnv opens the session whose directory GEAS_SESSION_DIR names: the
// credential helper and the sandbox check run inside a session, where the
// state directory around it may be unreadable.
func FromEnv() (*Dir, error) {
	value := os.Getenv("GEAS_SESSION_DIR")
	if value == "" {
		return nil, errors.New("not inside a geas session: GEAS_SESSION_DIR is not set")
	}
	if !filepath.IsAbs(value) || !sessionID.MatchString(filepath.Base(value)) {
		return nil, errors.New("GEAS_SESSION_DIR does not name a session directory")
	}
	return at(filepath.Clean(value))
}

// at checks that dir is a directory only its owner, the caller, can enter.
func at(dir string) (*Dir, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("session directory must be owned by you, a directory and chmod 700")
	}
	return &Dir{id: filepath.Base(dir), path: dir}, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// ID is the session's ID.
func (d *Dir) ID() string { return d.id }

// Path is the directory's absolute path, GEAS_SESSION_DIR inside the session.
func (d *Dir) Path() string { return d.path }

// Join returns the absolute path of rel, a path inside the directory, and
// refuses one that would leave it.
func (d *Dir) Join(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", errors.New("invalid path in the session directory")
	}
	return filepath.Join(d.path, rel), nil
}

// WriteFile replaces rel atomically, owner-only, creating the directories
// above it: a temporary file in the same directory, synced, then renamed, so
// that glab, gh or git never read half of a configuration.
func (d *Dir) WriteFile(rel string, raw []byte) error {
	path, err := d.Join(rel)
	if err != nil {
		return err
	}
	// Never recreate a session's directory once it is removed.
	if _, err = os.Lstat(d.path); err != nil {
		return err
	}
	return secretfile.Write(path, raw)
}

// ReadFile returns rel's contents, once it is an owner-only regular file.
func (d *Dir) ReadFile(rel string) ([]byte, error) {
	path, err := d.Join(rel)
	if err != nil {
		return nil, err
	}
	return secretfile.Read(path)
}

// Remove deletes the directory and everything in it, once nothing in the
// ledger is left to revoke.
func (d *Dir) Remove() error { return os.RemoveAll(d.path) }

// The states a provider's marker records: written pending when the session
// is planned, then ready or failed once the background acquisition ends. A
// provider without a marker is pending too.
const (
	Pending = "pending"
	Ready   = "ready"
	Failed  = "failed"
)

// Marker is how far one provider got: still acquiring, its credential in the
// directory and its tools' configuration rendered, or failed, for Reason. The
// reason is shown to the harness, so it names no local path and no secret.
type Marker struct {
	State  string    `json:"state"`
	Reason string    `json:"reason,omitempty"`
	Time   time.Time `json:"time"`
}

// providerID is what a provider ID may look like in a file name, such as
// gitlab:gitlab.com.
var providerID = regexp.MustCompile(`^[a-z][a-z0-9]*(:[A-Za-z0-9.-]+)?$`)

// ProviderFile is rel's path for one provider below a directory of the
// session's, such as markers/gitlab:gitlab.com.
func ProviderFile(directory, provider string) (string, error) {
	if !providerID.MatchString(provider) {
		return "", errors.New("invalid provider ID")
	}
	return directory + "/" + provider, nil
}

func validState(state string) bool { return state == Pending || state == Ready || state == Failed }

// SetMarker records a provider's state.
func (d *Dir) SetMarker(provider string, m Marker) error {
	rel, err := ProviderFile("markers", provider)
	if err != nil {
		return err
	}
	if !validState(m.State) {
		return errors.New("invalid marker state")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return d.WriteFile(rel, raw)
}

// Marker returns a provider's state; an error wrapping os.ErrNotExist means
// it is still pending.
func (d *Dir) Marker(provider string) (Marker, error) {
	rel, err := ProviderFile("markers", provider)
	if err != nil {
		return Marker{}, err
	}
	raw, err := d.ReadFile(rel)
	if err != nil {
		return Marker{}, err
	}
	var m Marker
	if json.Unmarshal(raw, &m) != nil || !validState(m.State) {
		return Marker{}, errors.New("invalid marker")
	}
	return m, nil
}
