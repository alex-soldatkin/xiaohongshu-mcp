package browser

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ulikunitz/xz"
)

// 内置浏览器的下载分发地址。
const browserCDNBase = "https://cdn.one-world.ai/browsers"

// browserVersion 是内置浏览器的唯一版本源。升级只改 browser_version.txt 一处，Go 与 Dockerfile 同读。
//
//go:embed browser_version.txt
var browserVersionRaw string

var browserVersion = strings.TrimSpace(browserVersionRaw)

// browserSHA256Raw pins the expected SHA256 of every platform asset for the
// version above. It is the trust anchor: the CDN serves both the archive and
// its SHA256SUMS, so a checksum taken from there proves transport integrity
// but says nothing about authenticity. A checksum committed in the repository
// can only change through a reviewable diff.
//
//go:embed browser_sha256.txt
var browserSHA256Raw string

// pinnedSHA256 maps asset filename -> expected lowercase hex digest.
var pinnedSHA256 = parsePinnedSHA256(browserSHA256Raw)

// parsePinnedSHA256 reads sha256sum-style lines ("<hash>  <filename>"),
// ignoring blank lines and '#' comments.
func parsePinnedSHA256(raw string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		out[fields[1]] = strings.ToLower(fields[0])
	}
	return out
}

func browserURL(name string) string {
	return browserCDNBase + "/" + browserVersion + "/" + name
}

// platformAsset 返回当前 OS/arch 对应的下载文件名与解压后二进制文件名。
// 第三个返回值为 false 表示当前平台无预编译二进制。
func platformAsset() (assetName, binName string, ok bool) {
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH != "arm64" {
			return "", "", false
		}
		return "macos-arm64.dmg", "Chromium", true
	case "linux":
		if runtime.GOARCH != "amd64" {
			return "", "", false
		}
		return "linux-x64.tar.xz", "chrome", true
	case "windows":
		if runtime.GOARCH != "amd64" {
			return "", "", false
		}
		return "windows-x64.zip", "chrome.exe", true
	}
	return "", "", false
}

func browserCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "xiaohongshu-mcp", "browser", browserVersion), nil
}

// EnsureBrowser 确保本地存在内置浏览器二进制，返回其路径。
// 已缓存则直接返回；否则下载 → 校验 SHA256 → 解压。当前平台无预编译二进制时返回 error。
func EnsureBrowser() (string, error) {
	asset, binName, ok := platformAsset()
	if !ok {
		return "", fmt.Errorf("当前平台 %s/%s 无预编译浏览器，暂不支持", runtime.GOOS, runtime.GOARCH)
	}

	cacheDir, err := browserCacheDir()
	if err != nil {
		return "", err
	}

	// 已缓存：遍历查找二进制
	if bin := findBinary(cacheDir, binName); bin != "" {
		return bin, nil
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}

	// 下载（重试 3 次）
	logrus.Infof("首次运行：下载内置浏览器 %s（%s，约 140-190MB，仅一次）...", browserVersion, asset)
	archivePath := filepath.Join(cacheDir, asset)
	var dlErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if dlErr = downloadFile(browserURL(asset), archivePath); dlErr == nil {
			break
		}
		logrus.Warnf("下载失败（第 %d/3 次）: %v", attempt, dlErr)
		_ = os.Remove(archivePath)
		time.Sleep(2 * time.Second)
	}
	if dlErr != nil {
		return "", fmt.Errorf("下载内置浏览器失败: %w\n"+
			"  本项目只用内置浏览器，缺它不继续。请检查网络后重试；\n"+
			"  离线环境可手动下载 %s，解压到 %s 后重启。", dlErr, browserURL(asset), cacheDir)
	}
	defer os.Remove(archivePath)

	// 校验 SHA256（本地已成分发点，必须校验完整性）
	if err := verifySHA256(archivePath, asset); err != nil {
		return "", fmt.Errorf("校验失败: %w", err)
	}

	logrus.Infof("解压内置浏览器 ...")
	if err := extractArchive(archivePath, cacheDir); err != nil {
		return "", fmt.Errorf("解压失败: %w", err)
	}

	bin := findBinary(cacheDir, binName)
	if bin == "" {
		return "", fmt.Errorf("解压后未找到二进制 %s", binName)
	}
	logrus.Infof("内置浏览器就绪: %s", bin)
	return bin, nil
}

// verifySHA256 verifies the downloaded archive against two independent sources,
// in order of trust:
//
//  1. the digest pinned in browser_sha256.txt, which ships with this source
//     tree — a mismatch is fatal, and a missing pin is fatal too (fail closed);
//  2. the CDN's SHA256SUMS, kept as a secondary check. It cannot add
//     authenticity — it comes from the same origin as the payload — but it
//     does catch a CDN whose archive and sums have drifted apart, which is a
//     signal worth surfacing. Network failure here is only a warning: the
//     pinned digest has already decided the question.
func verifySHA256(archivePath, asset string) error {
	got, err := sha256File(archivePath)
	if err != nil {
		return err
	}

	want, ok := pinnedSHA256[asset]
	if !ok {
		return fmt.Errorf("browser_sha256.txt 中未固定 %s 的哈希，拒绝使用未经校验的二进制", asset)
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%s SHA256 与仓库内固定值不匹配：期望 %s，实际 %s", asset, want, got)
	}

	cdn, err := fetchCDNSHA(asset)
	if err != nil {
		logrus.Warnf("无法获取 CDN SHA256SUMS 做二次核对（已通过仓库内固定值校验）: %v", err)
		return nil
	}
	if !strings.EqualFold(cdn, want) {
		return fmt.Errorf("CDN SHA256SUMS 与仓库内固定值不一致：CDN %s，固定值 %s。"+
			"下载的文件与固定值相符，但分发点已发生变化，请人工核实后再升级版本", cdn, want)
	}
	return nil
}

// sha256File returns the lowercase hex SHA256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchCDNSHA is indirected so tests can exercise the secondary check without
// reaching the network.
var fetchCDNSHA = fetchExpectedSHA

// fetchExpectedSHA downloads the SHA256SUMS file sitting next to the archive
// and pulls out the hash for asset.
func fetchExpectedSHA(asset string) (string, error) {
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(browserURL("SHA256SUMS"))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("获取 SHA256SUMS: HTTP %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		// 格式：<hash>␠␠<filename>
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[1] == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS 中未找到 %s", asset)
}

func findBinary(dir, binName string) string {
	var found string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) == binName {
			found = path
			return io.EOF // 提前结束
		}
		return nil
	})
	if found != "" {
		if err := os.Chmod(found, 0o755); err != nil {
			logrus.Debugf("chmod %s: %v", found, err)
		}
	}
	return found
}

func downloadFile(url, dst string) error {
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func extractArchive(archivePath, destDir string) error {
	switch {
	case strings.HasSuffix(archivePath, ".tar.xz"):
		return extractTarXz(archivePath, destDir)
	case strings.HasSuffix(archivePath, ".zip"):
		return extractZip(archivePath, destDir)
	case strings.HasSuffix(archivePath, ".dmg"):
		return extractDmg(archivePath, destDir)
	}
	return fmt.Errorf("不支持的压缩格式: %s", archivePath)
}

func extractTarXz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	xzr, err := xz.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(xzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Symlink(hdr.Linkname, target)
		}
	}
	return nil
}

func extractZip(archivePath, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		target := filepath.Join(destDir, zf.Name)
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, zf.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractDmg(archivePath, destDir string) error {
	mountPoint, err := os.MkdirTemp("", "bx-dmg-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mountPoint)

	if out, err := exec.Command("hdiutil", "attach", archivePath, "-nobrowse", "-mountpoint", mountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("hdiutil attach: %v: %s", err, out)
	}
	defer exec.Command("hdiutil", "detach", mountPoint, "-quiet").Run()

	var appPath string
	entries, _ := os.ReadDir(mountPoint)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".app") {
			appPath = filepath.Join(mountPoint, e.Name())
			break
		}
	}
	if appPath == "" {
		return fmt.Errorf("dmg 内未找到 .app")
	}
	dstApp := filepath.Join(destDir, filepath.Base(appPath))
	if out, err := exec.Command("cp", "-R", appPath, dstApp).CombinedOutput(); err != nil {
		return fmt.Errorf("拷贝 .app: %v: %s", err, out)
	}
	_ = exec.Command("xattr", "-dr", "com.apple.quarantine", dstApp).Run()
	return nil
}
