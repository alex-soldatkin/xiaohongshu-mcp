package browser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

// seedMarkerFile records which cookies.json snapshot a profile was last seeded
// from. It lives inside the profile directory rather than in the session file
// for two reasons: DeleteCookies removes the session file (which would take the
// marker with it), and a marker next to the profile it describes can be deleted
// on its own to force a reseed without wiping localStorage.
const seedMarkerFile = "xhs-seeded-from"

// seedMarker is the marker file's contents.
type seedMarker struct {
	// SeededFrom is the saved_at of the cookies.json this profile was seeded
	// from, so a later file can be recognised as newer.
	SeededFrom time.Time `json:"seeded_from"`
	// Seed is the fingerprint seed the profile's local state was minted under.
	// localStorage and the geometry belong to that fingerprint; changing the
	// seed under an existing profile is worth a loud warning.
	Seed int `json:"seed"`
}

func seedMarkerPath(profileDir string) string {
	return filepath.Join(profileDir, seedMarkerFile)
}

// readSeedMarker returns the marker and whether one was readable. A corrupt
// marker reads as absent, which forces a reseed from the file — safe, because
// seeding only ever overwrites cookies the profile already has.
func readSeedMarker(profileDir string) (seedMarker, bool) {
	data, err := os.ReadFile(seedMarkerPath(profileDir))
	if err != nil {
		return seedMarker{}, false
	}

	var m seedMarker
	if err := json.Unmarshal(data, &m); err != nil {
		logrus.Warnf("browser: unreadable seed marker in %s, treating the profile as unseeded: %v", profileDir, err)
		return seedMarker{}, false
	}
	return m, true
}

// writeSeedMarker writes the marker with write-then-rename, so a crash mid-write
// cannot leave a truncated file that reads back as "never seeded".
func writeSeedMarker(profileDir string, m seedMarker) error {
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	path := seedMarkerPath(profileDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeSeedMarker deletes the marker. Used by Reset, and available to an
// operator who wants to force a reseed from cookies.json.
func removeSeedMarker(profileDir string) error {
	err := os.Remove(seedMarkerPath(profileDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// seedPolicy decides whether the next launch should replay cookies.json into
// the browser. The profile is canonical; the file is a backup, and replaying a
// backup over a live jar is how the stale-cookie bug in #6 happens.
//
// fileSavedAt is the session file's saved_at, zero when it is a v1 bare array
// or unparsable.
func seedPolicy(marker seedMarker, hasMarker bool, fileSavedAt time.Time, hasCookies bool) (bool, string) {
	switch {
	case !hasMarker && !hasCookies:
		// A. First run ever: nothing to seed from, nothing to record yet.
		return false, "fresh profile and no session file"
	case !hasMarker:
		// B. Fresh (or never-seeded) profile with a session file: this is the
		// migration path from the old per-call temp profiles.
		return true, "profile has never been seeded from this session file"
	case !hasCookies:
		// F. The file was deleted but the profile still holds the login. Do not
		// wipe it; the next export recreates the file from the live jar.
		return false, "no session file; the profile keeps the session"
	case fileSavedAt.After(marker.SeededFrom):
		// D. Someone rewrote cookies.json out of band (cmd/login elsewhere, a
		// restored newer backup). The newer snapshot wins.
		return true, "session file is newer than the profile's last seed"
	case fileSavedAt.Equal(marker.SeededFrom):
		// C. Steady state, the overwhelmingly common case.
		return false, "profile already seeded from this session file"
	default:
		// E. An older backup was dropped over a live profile. Replaying it
		// would present cookies the server has already rotated.
		return false, "session file is older than the profile's last seed; delete " +
			seedMarkerFile + " in the profile dir to force a reseed"
	}
}

// Chrome's singleton files. SingletonLock is a symlink whose target is
// "<hostname>-<pid>"; the other two are the IPC socket and cookie.
var singletonFiles = []string{"SingletonLock", "SingletonSocket", "SingletonCookie"}

// clearStaleSingleton removes a leftover singleton lock so a profile whose
// owner is gone can be opened again.
//
// This cannot be left to Chrome. Its own dead-owner detection only works when
// the lock names the current hostname, and in Docker the hostname is the
// container id: recreate the container over the same volume and Chrome decides
// the profile is "in use on another computer" and refuses to start. That is a
// normal restart, not an error.
//
// When the lock does name this host and that pid is still alive, the owner is
// real — a second server or a cmd/login run — and stealing the lock would let
// two Chromes corrupt the same LevelDB files. That case returns an error
// naming the pid.
func clearStaleSingleton(profileDir string) error {
	lock := filepath.Join(profileDir, "SingletonLock")

	target, err := os.Readlink(lock)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no lock, or a platform that uses none
		}
		// Not a symlink, or unreadable: treat it as debris and remove it below.
		logrus.Warnf("browser: cannot read %s (%v), removing it", lock, err)
		target = ""
	}

	if host, pid, ok := parseSingletonTarget(target); ok {
		if self, err := os.Hostname(); err == nil && host == self && processAlive(pid) {
			return fmt.Errorf(
				"profile %s is locked by a live process (pid %d on %s): is another server or cmd/login using it?",
				profileDir, pid, host)
		}
		logrus.Infof("browser: clearing stale singleton lock in %s (owner %s pid %d is gone)", profileDir, host, pid)
	}

	for _, name := range singletonFiles {
		if err := os.Remove(filepath.Join(profileDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	return nil
}

// parseSingletonTarget splits "<hostname>-<pid>". The hostname may itself
// contain dashes, so the split is on the last one.
func parseSingletonTarget(target string) (string, int, bool) {
	i := strings.LastIndexByte(target, '-')
	if i <= 0 {
		return "", 0, false
	}
	pid, err := strconv.Atoi(target[i+1:])
	if err != nil || pid <= 0 {
		return "", 0, false
	}
	return target[:i], pid, true
}

// processAlive reports whether a pid is a live process, via signal 0.
//
// ESRCH means gone; EPERM means alive but owned by somebody else, which still
// counts as alive.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errorIsPermission(err)
}

func errorIsPermission(err error) bool {
	return os.IsPermission(err) || strings.Contains(err.Error(), "operation not permitted")
}
