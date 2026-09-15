package cookies

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetCookiesFilePath 校验路径优先级：COOKIES_PATH > 当前目录 > /tmp（旧路径兜底）。
// 用 TMPDIR 重定向 os.TempDir()、t.Chdir 重定向当前目录，做到 hermetic、不碰真实 /tmp。
func TestGetCookiesFilePath(t *testing.T) {
	t.Run("显式指定的COOKIES_PATH永远最优先", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("TMPDIR", dir)
		t.Setenv("COOKIES_PATH", "/custom/cookies.json")

		// 即使 /tmp 下躺着旧文件，也不能盖掉显式配置
		assert.NoError(t, os.WriteFile(filepath.Join(dir, "cookies.json"), []byte("[]"), 0644))

		assert.Equal(t, "/custom/cookies.json", GetCookiesFilePath())
	})

	t.Run("未设COOKIES_PATH时本地目录优先于tmp", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		t.Setenv("COOKIES_PATH", "")

		assert.NoError(t, os.WriteFile(filepath.Join(tmp, "cookies.json"), []byte("[]"), 0644))
		cwd := t.TempDir()
		t.Chdir(cwd)
		assert.NoError(t, os.WriteFile(filepath.Join(cwd, "cookies.json"), []byte("[]"), 0644))

		assert.Equal(t, "cookies.json", GetCookiesFilePath())
	})

	t.Run("本地没有时兜底到tmp旧路径", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		t.Setenv("COOKIES_PATH", "")
		t.Chdir(t.TempDir()) // 本地目录是空的

		oldPath := filepath.Join(tmp, "cookies.json")
		assert.NoError(t, os.WriteFile(oldPath, []byte("[]"), 0644))

		assert.Equal(t, oldPath, GetCookiesFilePath())
	})

	t.Run("都不存在时回退当前目录", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("TMPDIR", dir)
		t.Setenv("COOKIES_PATH", "")
		t.Chdir(t.TempDir())

		assert.Equal(t, "cookies.json", GetCookiesFilePath())
	})
}

// TestLoadSaveDeleteCookies 校验 cookie 文件存取往返与删除的幂等。
func TestLoadSaveDeleteCookies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")
	c := NewLoadCookie(path)

	// 未写入时读取应报错
	_, err := c.LoadCookies()
	assert.Error(t, err)

	// 写入后能读回同样的内容（落盘是 v2，排版会变，内容不变）
	want := []byte(`[{"name":"web_session","value":"x"}]`)
	assert.NoError(t, c.SaveCookies(want))
	got, err := c.LoadCookies()
	assert.NoError(t, err)
	assert.Equal(t, decodeJSON(t, want), decodeJSON(t, got))

	// 落盘是 v2 外层对象，不再是裸数组。只看结构，不看排版
	onDisk, err := os.ReadFile(path)
	assert.NoError(t, err)
	var f map[string]any
	assert.NoError(t, json.Unmarshal(onDisk, &f))
	assert.Equal(t, float64(2), f["version"])

	// 删除后文件消失，且再次删除幂等（不报错）
	assert.NoError(t, c.DeleteCookies())
	assert.NoFileExists(t, path)
	assert.NoError(t, c.DeleteCookies())
}

// TestSeed 校验 seed 的持久化与 v1/v2 两种格式的读取。
func TestSeed(t *testing.T) {
	t.Run("v1裸数组读不到seed且cookies原样", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		raw := []byte(`[{"name":"web_session","value":"x"}]`)
		assert.NoError(t, os.WriteFile(path, raw, 0644))

		c := NewLoadCookie(path)

		got, err := c.LoadCookies()
		assert.NoError(t, err)
		assert.Equal(t, decodeJSON(t, raw), decodeJSON(t, got))
		assert.Equal(t, 0, c.LoadSeed())
	})

	t.Run("存seed后读得回来且cookies内容不走样", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		c := NewLoadCookie(path)

		// 字段顺序刻意打乱、数值用科学计数法，用来暴露"反序列化再序列化"造成的走样
		raw := []byte(`[{"value":"x","name":"web_session","expires":1.75e9}]`)
		assert.NoError(t, c.SaveCookies(raw))
		assert.NoError(t, c.SaveSeed(23088))

		assert.Equal(t, 23088, c.LoadSeed())

		got, err := c.LoadCookies()
		assert.NoError(t, err)
		// 比语义不比字节：落盘会重新缩进（只动无意义空白），字段和取值一个都不能变
		assert.Equal(t, decodeJSON(t, raw), decodeJSON(t, got))
	})
}

// TestSeedRobustness 校验 seed 在异常与并发写入下的表现。
func TestSeedRobustness(t *testing.T) {
	t.Run("存cookies不冲掉已有的seed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		c := NewLoadCookie(path)

		assert.NoError(t, c.SaveSeed(23088))
		// 模拟重新登录：cookies 换了一批，seed 必须留着
		assert.NoError(t, c.SaveCookies([]byte(`[{"name":"web_session","value":"new"}]`)))

		assert.Equal(t, 23088, c.LoadSeed())
	})

	t.Run("文件损坏时降级为没有seed且不panic", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		assert.NoError(t, os.WriteFile(path, []byte(`{"version":2,"seed":`), 0644))

		assert.Equal(t, 0, NewLoadCookie(path).LoadSeed())
	})

	t.Run("文件不存在时读seed返回0", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nope.json")

		assert.Equal(t, 0, NewLoadCookie(path).LoadSeed())
	})
}

// decodeJSON 把 JSON 解成通用结构，用于只比内容、不比排版。
func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()

	var v any
	assert.NoError(t, json.Unmarshal(data, &v))
	return v
}

// TestSaveCookies_CreatesParentDir 保存时父目录不存在也应能落盘。
//
// COOKIES_PATH 指向一个还没创建的目录时，原先会直接写失败。
func TestSaveCookies_CreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "cookies.json")

	c := NewLoadCookie(path)
	assert.NoError(t, c.SaveCookies([]byte(`[{"name":"a","value":"b"}]`)))

	got, err := c.LoadCookies()
	assert.NoError(t, err)

	var cks []map[string]string
	assert.NoError(t, json.Unmarshal(got, &cks))
	assert.Equal(t, "a", cks[0]["name"])
}

// TestLoadSavedAt covers the three shapes a session file can have: v2 with a
// timestamp, v1 (a bare cookie array, no timestamp at all) and no file.
func TestLoadSavedAt(t *testing.T) {
	t.Run("v2 file reports when it was written", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		store := NewLoadCookie(path)

		before := time.Now().Add(-time.Second)
		require.NoError(t, store.SaveCookies([]byte(`[{"name":"a"}]`)))
		after := time.Now().Add(time.Second)

		got := store.LoadSavedAt()
		require.False(t, got.IsZero(), "saved_at must be set by SaveCookies")
		assert.True(t, got.After(before) && got.Before(after), "saved_at %s outside [%s, %s]", got, before, after)
	})

	t.Run("v1 bare array has no timestamp", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		require.NoError(t, os.WriteFile(path, []byte(`[{"name":"a"}]`), 0o644))

		assert.True(t, NewLoadCookie(path).LoadSavedAt().IsZero())
	})

	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		assert.True(t, NewLoadCookie(path).LoadSavedAt().IsZero())
	})

	t.Run("unparsable timestamp degrades to zero", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"version":2,"saved_at":"yesterday","cookies":[]}`), 0o644))

		assert.True(t, NewLoadCookie(path).LoadSavedAt().IsZero())
	})

	t.Run("saved_at round-trips through RFC3339 truncation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		store := NewLoadCookie(path)
		require.NoError(t, store.SaveCookies([]byte(`[]`)))

		first := store.LoadSavedAt()
		assert.Equal(t, first.Format(time.RFC3339), store.LoadSavedAt().Format(time.RFC3339))
	})
}

// TestSite covers the site field: recorded once, preserved by every other
// write, and absent in the files written before it existed (issue #18).
func TestSite(t *testing.T) {
	t.Run("存站点后读得回来，且不冲掉 cookies 与 seed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		c := NewLoadCookie(path)

		raw := []byte(`[{"name":"web_session","value":"x","domain":".rednote.com"}]`)
		assert.NoError(t, c.SaveCookies(raw))
		assert.NoError(t, c.SaveSeed(23088))
		assert.NoError(t, c.SaveSite("rednote"))

		assert.Equal(t, "rednote", c.LoadSite())
		assert.Equal(t, 23088, c.LoadSeed())
		got, err := c.LoadCookies()
		assert.NoError(t, err)
		assert.Equal(t, decodeJSON(t, raw), decodeJSON(t, got))
	})

	t.Run("重新导出 cookies 不冲掉站点", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		c := NewLoadCookie(path)

		assert.NoError(t, c.SaveSite("rednote"))
		assert.NoError(t, c.SaveCookies([]byte(`[{"name":"web_session","value":"new"}]`)))
		assert.NoError(t, c.SaveSeed(1))

		assert.Equal(t, "rednote", c.LoadSite())
	})

	t.Run("老文件与损坏文件读站点返回空", func(t *testing.T) {
		dir := t.TempDir()

		v1 := filepath.Join(dir, "v1.json")
		assert.NoError(t, os.WriteFile(v1, []byte(`[{"name":"web_session"}]`), 0644))
		assert.Equal(t, "", NewLoadCookie(v1).LoadSite())

		broken := filepath.Join(dir, "broken.json")
		assert.NoError(t, os.WriteFile(broken, []byte(`{"version":2,"site":`), 0644))
		assert.Equal(t, "", NewLoadCookie(broken).LoadSite())

		assert.Equal(t, "", NewLoadCookie(filepath.Join(dir, "nope.json")).LoadSite())
	})

	t.Run("没有站点时不写出 site 字段", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies.json")
		assert.NoError(t, NewLoadCookie(path).SaveCookies([]byte(`[]`)))

		data, err := os.ReadFile(path)
		assert.NoError(t, err)
		assert.NotContains(t, string(data), `"site"`)
	})
}
