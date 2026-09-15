# Bumping the bundled browser version

The project ships a patched Chromium from `https://cdn.one-world.ai/browsers/<version>/`.
Two files in `browser/` decide what gets installed, and both are read by Go
(`go:embed`) and by the Dockerfile:

- `browser/browser_version.txt` — the version string, the only source of truth for it.
- `browser/browser_sha256.txt` — the expected SHA256 of each platform asset.

## Why the checksum lives here

The CDN serves the archive and the `SHA256SUMS` file next to it from the same
origin. Anyone able to replace the archive can replace the sums, so a checksum
fetched at install time proves the download was not mangled in transit and
nothing more. The binary in question holds a logged-in Xiaohongshu session, so
the trust anchor belongs somewhere a change is visible: a digest committed to
this repository changes only through a diff someone reviews.

`verifySHA256` in `browser/browser_download.go` therefore checks the embedded
digest first and treats a mismatch — or a missing pin — as fatal. The CDN's
`SHA256SUMS` is still consulted afterwards, not as authority but as a drift
alarm: if the distribution point's sums stop agreeing with the pin, the build
fails loudly even though the bytes on disk were correct. An unreachable CDN at
that point is only a warning. The Dockerfile enforces the same two checks for
the Linux asset.

Pinning cannot bootstrap itself: the first time a digest is recorded it comes
from the CDN, and that one fetch is trusted. What pinning buys is every run
after it.

## Procedure

1. Publish (or confirm) the new version's three assets on the CDN:
   `macos-arm64.dmg`, `linux-x64.tar.xz`, `windows-x64.zip`.

2. Download all three and compute the digests yourself. Do not copy the CDN's
   `SHA256SUMS` blindly — compute from the bytes you actually received, then
   compare against the CDN file as a cross-check:

   ```sh
   VER=<new-version>
   BASE="https://cdn.one-world.ai/browsers/${VER}"
   for a in macos-arm64.dmg linux-x64.tar.xz windows-x64.zip; do
     curl -fsSL -o "/tmp/$a" "$BASE/$a" && shasum -a 256 "/tmp/$a"
   done
   curl -fsSL "$BASE/SHA256SUMS"
   ```

3. Sanity-check the artefacts before trusting them — at minimum, unpack the one
   for your platform and confirm `--version` reports the version you expect.

4. Update `browser/browser_version.txt` and `browser/browser_sha256.txt` in the
   **same commit**. A commit that moves one without the other leaves the tree in
   a state where every install fails, which is the intended fail-closed
   behaviour but a poor way to discover it.

5. Run `go test ./browser/` and rebuild the Docker image. Delete the local cache
   (`$XDG_CACHE_HOME/xiaohongshu-mcp/browser/`, on macOS
   `~/Library/Caches/xiaohongshu-mcp/browser/`) to exercise the download path
   end to end rather than the cache hit.

6. In the commit message, record where the digests came from and that they were
   computed locally. That line is what a future reviewer has to go on.
