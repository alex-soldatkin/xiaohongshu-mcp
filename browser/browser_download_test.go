package browser

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pinned digests are the trust anchor for the bundled browser; if one goes
// missing, EnsureBrowser must fail closed rather than fall back to the CDN.
func TestPinnedSHA256CoversEveryPlatformAsset(t *testing.T) {
	for _, asset := range []string{"macos-arm64.dmg", "linux-x64.tar.xz", "windows-x64.zip"} {
		digest, ok := pinnedSHA256[asset]
		if !ok {
			t.Fatalf("browser_sha256.txt 未固定 %s 的哈希", asset)
		}
		if len(digest) != 64 {
			t.Fatalf("%s: 期望 64 位十六进制摘要，实际 %q", asset, digest)
		}
		if strings.Trim(digest, "0123456789abcdef") != "" {
			t.Fatalf("%s: 摘要含非十六进制字符: %q", asset, digest)
		}
	}
}

func TestParsePinnedSHA256IgnoresCommentsAndBlanks(t *testing.T) {
	got := parsePinnedSHA256("# a comment\n\n  aa11  foo.zip\nbb22  bar.tar.xz\n")
	if len(got) != 2 {
		t.Fatalf("期望 2 条记录，实际 %d: %v", len(got), got)
	}
	if got["foo.zip"] != "aa11" || got["bar.tar.xz"] != "bb22" {
		t.Fatalf("解析结果错误: %v", got)
	}
}

// Corrupting a single byte of the archive must be rejected, and rejected by the
// embedded pin alone — without consulting the CDN.
func TestVerifySHA256RejectsCorruptedByte(t *testing.T) {
	const asset = "test-asset.bin"
	dir := t.TempDir()
	path := filepath.Join(dir, asset)

	payload := []byte("pretend this is a 150MB chromium archive")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}

	pinnedSHA256[asset] = digest
	t.Cleanup(func() { delete(pinnedSHA256, asset) })

	cdnCalls := 0
	restore := fetchCDNSHA
	fetchCDNSHA = func(string) (string, error) { cdnCalls++; return digest, nil }
	t.Cleanup(func() { fetchCDNSHA = restore })

	if err := verifySHA256(path, asset); err != nil {
		t.Fatalf("未篡改的文件应通过校验: %v", err)
	}

	payload[7] ^= 0x01 // flip one bit of one byte
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	cdnCalls = 0
	err = verifySHA256(path, asset)
	if err == nil {
		t.Fatal("篡改一个字节后仍通过校验")
	}
	if !strings.Contains(err.Error(), "不匹配") {
		t.Fatalf("期望哈希不匹配错误，实际: %v", err)
	}
	if cdnCalls != 0 {
		t.Fatalf("固定值不匹配时不应再请求 CDN，实际请求 %d 次", cdnCalls)
	}
}

func TestVerifySHA256RejectsUnpinnedAsset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unknown.bin")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySHA256(path, "unknown.bin"); err == nil {
		t.Fatal("未固定哈希的文件应被拒绝")
	}
}

// A CDN whose SHA256SUMS has drifted away from the pin is reported even when the
// downloaded bytes match the pin: the archive is fine, the distribution point is not.
func TestVerifySHA256ReportsCDNDrift(t *testing.T) {
	const asset = "drift-asset.bin"
	dir := t.TempDir()
	path := filepath.Join(dir, asset)
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	pinnedSHA256[asset] = digest
	t.Cleanup(func() { delete(pinnedSHA256, asset) })

	restore := fetchCDNSHA
	t.Cleanup(func() { fetchCDNSHA = restore })

	fetchCDNSHA = func(string) (string, error) {
		return strings.Repeat("ab", 32), nil
	}
	if err := verifySHA256(path, asset); err == nil {
		t.Fatal("CDN 哈希与固定值不一致时应报错")
	}

	// A CDN that is merely unreachable must not block a locally verified archive.
	fetchCDNSHA = func(string) (string, error) { return "", errors.New("network down") }
	if err := verifySHA256(path, asset); err != nil {
		t.Fatalf("CDN 不可达不应影响已通过固定值校验的文件: %v", err)
	}
}
