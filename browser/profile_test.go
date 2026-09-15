package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSeedPolicy walks every case of the seeding state machine from issue #6.
// The rule being pinned is that the profile is canonical: cookies.json is
// replayed only when it is demonstrably newer than what the profile already
// holds, because replaying a stale backup presents cookies the server has
// already rotated -- the original bug.
func TestSeedPolicy(t *testing.T) {
	seededAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		marker     seedMarker
		hasMarker  bool
		savedAt    time.Time
		hasCookies bool
		wantSeed   bool
	}{
		{name: "A 首次运行：无 marker 无文件", wantSeed: false},
		{name: "B 新 profile + 已有会话文件", savedAt: seededAt, hasCookies: true, wantSeed: true},
		{
			name: "C 稳态：marker 与文件同一时刻",
			marker: seedMarker{SeededFrom: seededAt}, hasMarker: true,
			savedAt: seededAt, hasCookies: true, wantSeed: false,
		},
		{
			name: "D 文件在别处被重写，比 marker 新",
			marker: seedMarker{SeededFrom: seededAt}, hasMarker: true,
			savedAt: seededAt.Add(time.Hour), hasCookies: true, wantSeed: true,
		},
		{
			name: "E 旧备份盖在活着的 profile 上",
			marker: seedMarker{SeededFrom: seededAt}, hasMarker: true,
			savedAt: seededAt.Add(-time.Hour), hasCookies: true, wantSeed: false,
		},
		{
			name: "F 文件被删，profile 仍登录着",
			marker: seedMarker{SeededFrom: seededAt}, hasMarker: true,
			hasCookies: false, wantSeed: false,
		},
		{
			name:    "G DeleteCookies 之后等同于 A",
			savedAt: time.Time{}, hasCookies: false, wantSeed: false,
		},
		{
			name:    "v1 裸数组文件（无 saved_at）仍会给新 profile 播种",
			savedAt: time.Time{}, hasCookies: true, wantSeed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seed, reason := seedPolicy(tt.marker, tt.hasMarker, tt.savedAt, tt.hasCookies)
			assert.Equal(t, tt.wantSeed, seed, "reason: %s", reason)
			assert.NotEmpty(t, reason, "every decision must explain itself in the log")
		})
	}

	t.Run("E 的理由要告诉运维怎么强制重播", func(t *testing.T) {
		_, reason := seedPolicy(seedMarker{SeededFrom: seededAt}, true, seededAt.Add(-time.Hour), true)
		assert.Contains(t, reason, seedMarkerFile)
	})
}

func TestSeedMarkerRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")

	_, ok := readSeedMarker(dir)
	assert.False(t, ok, "a directory that does not exist yet has no marker")

	want := seedMarker{SeededFrom: time.Now().Truncate(time.Second).UTC(), Seed: 98759}
	require.NoError(t, writeSeedMarker(dir, want))

	got, ok := readSeedMarker(dir)
	require.True(t, ok)
	assert.Equal(t, want.Seed, got.Seed)
	assert.True(t, want.SeededFrom.Equal(got.SeededFrom), "%s != %s", want.SeededFrom, got.SeededFrom)

	// The written timestamp must compare Equal to the one the policy will read
	// out of the session file, or the steady state would reseed every launch.
	assert.False(t, got.SeededFrom.After(want.SeededFrom))
	assert.False(t, got.SeededFrom.Before(want.SeededFrom))

	t.Run("写入是原子的：不留 .tmp 残骸", func(t *testing.T) {
		_, err := os.Stat(seedMarkerPath(dir) + ".tmp")
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("损坏的 marker 视为不存在", func(t *testing.T) {
		require.NoError(t, os.WriteFile(seedMarkerPath(dir), []byte("{not json"), 0o644))
		_, ok := readSeedMarker(dir)
		assert.False(t, ok)
	})

	t.Run("删除 marker 是幂等的", func(t *testing.T) {
		require.NoError(t, removeSeedMarker(dir))
		require.NoError(t, removeSeedMarker(dir))
		_, ok := readSeedMarker(dir)
		assert.False(t, ok)
	})
}

// writeSingleton plants a SingletonLock symlink with the given target, the way
// Chrome does.
func writeSingleton(t *testing.T, dir, target string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, name := range []string{"SingletonSocket", "SingletonCookie"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o644))
	}
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "SingletonLock")))
}

func singletonExists(t *testing.T, dir string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(dir, "SingletonLock"))
	return err == nil
}

func TestClearStaleSingleton(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)

	t.Run("没有锁时是空操作", func(t *testing.T) {
		dir := t.TempDir()
		assert.NoError(t, clearStaleSingleton(dir))
	})

	t.Run("本机上活着的 pid：拒绝，并说出 pid", func(t *testing.T) {
		dir := t.TempDir()
		writeSingleton(t, dir, fmt.Sprintf("%s-%d", host, os.Getpid()))

		err := clearStaleSingleton(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), fmt.Sprintf("pid %d", os.Getpid()))
		assert.True(t, singletonExists(t, dir), "a live owner's lock must not be stolen")
	})

	t.Run("本机上已死的 pid：清掉", func(t *testing.T) {
		dir := t.TempDir()
		writeSingleton(t, dir, fmt.Sprintf("%s-%d", host, 999999))

		require.NoError(t, clearStaleSingleton(dir))
		assert.False(t, singletonExists(t, dir))
		for _, name := range []string{"SingletonSocket", "SingletonCookie"} {
			_, err := os.Stat(filepath.Join(dir, name))
			assert.True(t, os.IsNotExist(err), "%s left behind", name)
		}
	})

	t.Run("别的主机名：清掉（Docker 重建容器就是这一路）", func(t *testing.T) {
		dir := t.TempDir()
		writeSingleton(t, dir, "otherhost-1")

		require.NoError(t, clearStaleSingleton(dir))
		assert.False(t, singletonExists(t, dir))
	})

	t.Run("带连字符的主机名按最后一个连字符切分", func(t *testing.T) {
		h, pid, ok := parseSingletonTarget("my-build-box-4242")
		require.True(t, ok)
		assert.Equal(t, "my-build-box", h)
		assert.Equal(t, 4242, pid)
	})

	t.Run("无法解析的锁内容当垃圾清掉", func(t *testing.T) {
		dir := t.TempDir()
		writeSingleton(t, dir, "garbage")

		require.NoError(t, clearStaleSingleton(dir))
		assert.False(t, singletonExists(t, dir))
	})
}

func TestProcessAlive(t *testing.T) {
	assert.True(t, processAlive(os.Getpid()))
	assert.False(t, processAlive(999999))
	assert.False(t, processAlive(0), "pid 0 is not a process we can own")
}
