package pacing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"
)

// stateVersion guards against silently misreading a future format.
const stateVersion = 1

// persistedState is the on-disk shape of the counters.
//
// Deliberately dumb: one small JSON blob rewritten on every recorded action.
// Issue #7 replaces this with a real store; until then the only requirement is
// that a restart must not hand the account a fresh daily budget.
type persistedState struct {
	Version int `json:"version"`
	// Events holds action timestamps in unix milliseconds, per class.
	Events map[Class][]int64 `json:"events"`
	// CooldownUntil is a unix millisecond timestamp, 0 when not cooling down.
	CooldownUntil int64 `json:"cooldown_until,omitempty"`
}

func loadState(path string) persistedState {
	empty := persistedState{Version: stateVersion, Events: map[Class][]int64{}}
	if path == "" {
		return empty
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logrus.Warnf("pacing: cannot read state file %s: %v", path, err)
		}
		return empty
	}

	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion {
		logrus.Warnf("pacing: unusable state file %s (err=%v version=%d), starting from empty counters", path, err, st.Version)
		return empty
	}
	if st.Events == nil {
		st.Events = map[Class][]int64{}
	}
	return st
}

func saveState(path string, st persistedState) error {
	if path == "" {
		return nil
	}
	st.Version = stateVersion

	data, err := json.Marshal(st)
	if err != nil {
		return err
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	// Write-then-rename so a crash mid-write cannot leave a truncated file that
	// would be read back as "no actions today".
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func toMillis(ts []time.Time) []int64 {
	out := make([]int64, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.UnixMilli())
	}
	return out
}

func fromMillis(ms []int64) []time.Time {
	out := make([]time.Time, 0, len(ms))
	for _, m := range ms {
		out = append(out, time.UnixMilli(m))
	}
	return out
}
