🟠 **High** `preview/handlers.ts:57`

`writeScreenshotFile` writes directly to `absolutePath` after `WorkspacePaths.resolveRelativePathWithinRoot` validates it, but that validation only does lexical path resolution and does not resolve symlinks. If `<workspace>/out` is a symlink to `/tmp`, saving to `out/victim.png` causes `writeScreenshotFile` to overwrite `/tmp/victim.png` — a file outside the workspace. The containment check needs to resolve symlinks (e.g., via `realpath` on the destination or nearest existing parent) or use an open strategy that refuses to follow symlinks.

<details>
<summary>Also found in 1 other location(s)</summary>

`apps/server/src/assets/AssetAccess.ts:453`

> The `browser-artifact` resolver validates only the lexical basename and then uses `statIsFile` on `path.join(config.browserArtifactsDir, artifactFileName)`. Because `stat` and the subsequent file response follow symbolic links, a symlink such as `browser-artifacts/leak.png -&gt; ../state.sqlite` is accepted and served by a valid signed artifact token, exposing arbitrary files readable by the server. Canonicalize both the artifacts directory and candidate and verify the candidate remains inside the directory (as the workspace-file path does), or reject symlinks.


</details>



<details>
<summary>🚀 Reply "<strong>fix it for me</strong>" or copy this <strong>AI Prompt</strong> for your agent:</summary>

```text
In file @apps/server/src/mcp/toolkits/preview/handlers.ts around line 57:

`writeScreenshotFile` writes directly to `absolutePath` after `WorkspacePaths.resolveRelativePathWithinRoot` validates it, but that validation only does lexical path resolution and does not resolve symlinks. If `<workspace>/out` is a symlink to `/tmp`, saving to `out/victim.png` causes `writeScreenshotFile` to overwrite `/tmp/victim.png` — a file outside the workspace. The containment check needs to resolve symlinks (e.g., via `realpath` on the destination or nearest existing parent) or use an open strategy that refuses to follow symlinks.

Also found in 1 other location(s):
- apps/server/src/assets/AssetAccess.ts:453 -- The `browser-artifact` resolver validates only the lexical basename and then uses `statIsFile` on `path.join(config.browserArtifactsDir, artifactFileName)`. Because `stat` and the subsequent file response follow symbolic links, a symlink such as `browser-artifacts/leak.png -> ../state.sqlite` is accepted and served by a valid signed artifact token, exposing arbitrary files readable by the server. Canonicalize both the artifacts directory and candidate and verify the candidate remains inside the directory (as the workspace-file path does), or reject symlinks.

```
</details>