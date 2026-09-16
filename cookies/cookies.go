package cookies

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
)

// sessionFile 是 v2 的文件结构。v1 是一个裸 cookie 数组，没有外层对象。
// cookies 用 RawMessage 原样透传，不解析不重组，避免往返时字段走样。
type sessionFile struct {
	Version int    `json:"version"`
	Seed    int    `json:"seed,omitempty"`
	SavedAt string `json:"saved_at,omitempty"`
	// Site records which deployment (xiaohongshu / rednote) the jar belongs
	// to, so an operator who has logged in once never has to say it again.
	// Absent in jars written before the field existed; those are sniffed.
	Site    string          `json:"site,omitempty"`
	Cookies json.RawMessage `json:"cookies"`
}

// localCookiesPath 当前目录下的默认文件名。
const localCookiesPath = "cookies.json"

type Cookier interface {
	LoadCookies() ([]byte, error)
	SaveCookies(data []byte) error
	DeleteCookies() error
	// LoadSeed 读取会话绑定的 seed；老格式、文件损坏或未设时返回 0。
	LoadSeed() int
	// SaveSeed 写入 seed，保留文件中已有的 cookies。
	SaveSeed(seed int) error
	// LoadSavedAt reports when the session file was last written, parsed from
	// the v2 saved_at field. A v1 file (bare array), a missing file or an
	// unparsable timestamp all yield the zero time.
	//
	// The persistent profile (issue #6) uses this to decide whether the file
	// is newer than what the profile was last seeded from.
	LoadSavedAt() time.Time
	// LoadSite reads the deployment name recorded with the session. An older
	// file, a missing file or a damaged one all yield "".
	LoadSite() string
	// SaveSite records the deployment name, preserving cookies and seed.
	SaveSite(site string) error
}

type localCookie struct {
	path string
}

func NewLoadCookie(path string) Cookier {
	if path == "" {
		panic("path is required")
	}

	return &localCookie{
		path: path,
	}
}

// LoadCookies 从文件中加载 cookies 数组的原始字节。
// v2 从外层对象里取出 cookies 字段；v1 文件本身就是数组，原样返回。
func (c *localCookie) LoadCookies() ([]byte, error) {

	data, err := os.ReadFile(c.path)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read cookies from tmp file")
	}

	var f sessionFile
	if err := json.Unmarshal(data, &f); err == nil && len(f.Cookies) > 0 {
		return f.Cookies, nil
	}

	return data, nil
}

// LoadSeed 读取会话绑定的 seed。老格式（裸数组）没有这个值，返回 0。
func (c *localCookie) LoadSeed() int {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return 0
	}

	var f sessionFile
	if err := json.Unmarshal(data, &f); err != nil {
		return 0
	}
	return f.Seed
}

// LoadSavedAt reads saved_at from a v2 file. It returns the zero time for the
// old format, a missing file, or an unparsable timestamp.
func (c *localCookie) LoadSavedAt() time.Time {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return time.Time{}
	}

	var f sessionFile
	if err := json.Unmarshal(data, &f); err != nil || f.SavedAt == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339, f.SavedAt)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// LoadSite reads the site name bound to the session. It returns "" for the old
// format or when the field is unset.
func (c *localCookie) LoadSite() string {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return ""
	}

	var f sessionFile
	if err := json.Unmarshal(data, &f); err != nil {
		return ""
	}
	return f.Site
}

// SaveSite writes the site name, preserving the cookies and seed already in the
// file.
func (c *localCookie) SaveSite(site string) error {
	cks, err := c.LoadCookies()
	if err != nil {
		cks = nil // File does not exist yet: record the site now, cookies later.
	}
	return c.write(cks, c.LoadSeed(), site)
}

// SaveCookies writes the cookies to the file, preserving the seed and site name
// already in it.
func (c *localCookie) SaveCookies(data []byte) error {
	return c.write(data, c.LoadSeed(), c.LoadSite())
}

// SaveSeed 写入 seed，保留文件里已有的 cookies。
func (c *localCookie) SaveSeed(seed int) error {
	cks, err := c.LoadCookies()
	if err != nil {
		cks = nil // 文件还不存在：先把 seed 落下来，cookies 之后再补
	}
	return c.write(cks, seed, c.LoadSite())
}

// write 以 v2 格式落盘。cookies 用 RawMessage 原样嵌入，不经过结构体往返。
func (c *localCookie) write(cks []byte, seed int, site string) error {
	if len(cks) == 0 {
		cks = []byte("[]")
	}

	data, err := json.MarshalIndent(sessionFile{
		Version: 2,
		Seed:    seed,
		SavedAt: time.Now().Format(time.RFC3339),
		Site:    site,
		Cookies: json.RawMessage(cks),
	}, "", "  ")
	if err != nil {
		return errors.Wrap(err, "marshal session file failed")
	}

	if dir := filepath.Dir(c.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errors.Wrap(err, "create cookies dir failed")
		}
	}

	return os.WriteFile(c.path, data, 0644)
}

// DeleteCookies 删除 cookies 文件。
func (c *localCookie) DeleteCookies() error {
	if _, err := os.Stat(c.path); os.IsNotExist(err) {
		// 文件不存在，返回 nil（认为已经删除）
		return nil
	}
	return os.Remove(c.path)
}

// GetCookiesFilePath 获取 cookies 文件路径。
// 为了向后兼容，如果旧路径 /tmp/cookies.json 存在，则继续使用；
// 否则使用当前目录下的 cookies.json
func GetCookiesFilePath() string {
	// 显式指定优先，无条件——环境里的残留文件不能盖掉用户明说的配置
	if path := os.Getenv("COOKIES_PATH"); path != "" {
		return path
	}

	// 本地目录
	if _, err := os.Stat(localCookiesPath); err == nil {
		return localCookiesPath
	}

	// 旧路径 /tmp/cookies.json，仅为老用户兜底
	oldPath := filepath.Join(os.TempDir(), "cookies.json")
	if _, err := os.Stat(oldPath); err == nil {
		return oldPath
	}

	return localCookiesPath
}
