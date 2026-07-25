<!-- This is an auto-generated comment: summarize by coderabbit.ai -->
<!-- This is an auto-generated comment: rate limited by coderabbit.ai -->

> [!IMPORTANT]
> ## Review skipped
> 
> Too many files!
> 
> This PR contains 119 files, which is 19 over the limit of 100.
> 
> To get a review, narrow the scope:
>   • coderabbit review --committed          # exclude uncommitted changes
>   • coderabbit review --dir <path>         # limit to a subdirectory
>   • coderabbit review --base <branch>      # compare against a closer base
> 
> Upgrade to a paid plan to raise the limit.
> 
> This review couldn't start because sufficient usage credits or metered capacity aren't available. Add credits or update usage-based reviews in the [billing tab](https://app.coderabbit.ai/settings/billing?tab=usage&orgId=c5049a78-cc9d-4254-a553-d725b981bcca), then retry.
> 
> <details>
> <summary>⚙️ Run configuration</summary>
> 
> **Configuration used**: Organization UI
> 
> **Review profile**: ASSERTIVE
> 
> **Plan**: Pro Plus
> 
> **Run ID**: `72478289-4b6b-4c59-b012-cf5dbd7c1d5a`
> 
> </details>
> 
> <details>
> <summary>📥 Commits</summary>
> 
> Reviewing files that changed from the base of the PR and between a47f71d4a8d125f7c01668f17043eb090617b728 and 56150a0423a243224b03f355c3a3ba6941011b5b.
> 
> </details>
> 
> <details>
> <summary>⛔ Files ignored due to path filters (2)</summary>
> 
> * `bun.lock` is excluded by `!**/*.lock`
> * `src-tauri/Cargo.lock` is excluded by `!**/*.lock`
> 
> </details>
> 
> <details>
> <summary>📒 Files selected for processing (119)</summary>
> 
> * `.github/workflows/ci.yml`
> * `.github/workflows/release.yml`
> * `biome.jsonc`
> * `docs/ui-performance-baseline.md`
> * `package.json`
> * `scripts/benchmark-result-batching.ts`
> * `scripts/benchmark-ui-filter.ts`
> * `scripts/patch-updater-manifest.sh`
> * `src-tauri/Cargo.toml`
> * `src-tauri/examples/backend_bench.rs`
> * `src-tauri/src/commands/history.rs`
> * `src-tauri/src/commands/mod.rs`
> * `src-tauri/src/commands/playlist.rs`
> * `src-tauri/src/commands/recent.rs`
> * `src-tauri/src/commands/saved.rs`
> * `src-tauri/src/commands/scan.rs`
> * `src-tauri/src/commands/server_test.rs`
> * `src-tauri/src/commands/settings.rs`
> * `src-tauri/src/commands/updater.rs`
> * `src-tauri/src/engine/cast_proxy.rs`
> * `src-tauri/src/engine/checker.rs`
> * `src-tauri/src/engine/chromecast.rs`
> * `src-tauri/src/engine/ffmpeg.rs`
> * `src-tauri/src/engine/macos_dmg_update.rs`
> * `src-tauri/src/engine/mod.rs`
> * `src-tauri/src/engine/parser.rs`
> * `src-tauri/src/engine/playlist_score.rs`
> * `src-tauri/src/engine/proxy_common.rs`
> * `src-tauri/src/engine/resume.rs`
> * `src-tauri/src/engine/stream_proxy.rs`
> * `src-tauri/src/engine/xtream.rs`
> * `src-tauri/src/lib.rs`
> * `src-tauri/src/models/scan.rs`
> * `src-tauri/src/models/settings.rs`
> * `src-tauri/src/state.rs`
> * `src-tauri/tauri.conf.json`
> * `src-tauri/tauri.conf.release.json`
> * `src-tauri/tests/scan_pipeline_integration.rs`
> * `src/App.tsx`
> * `src/SettingsWindow.tsx`
> * `src/components/AppBanners.tsx`
> * `src/components/CastMenu.tsx`
> * `src/components/ChannelRow.tsx`
> * `src/components/ChannelTable.tsx`
> * `src/components/ErrorBoundary.tsx`
> * `src/components/ExportMenu.tsx`
> * `src/components/FilterBar.tsx`
> * `src/components/HistoryPanel.tsx`
> * `src/components/KeyboardShortcutsDialog.tsx`
> * `src/components/LogWindowContent.tsx`
> * `src/components/OpenSourceDialog.tsx`
> * `src/components/PasswordField.tsx`
> * `src/components/PlaylistReportPanel.tsx`
> * `src/components/ProgressBar.tsx`
> * `src/components/SFSymbols.tsx`
> * `src/components/SavedPlaylistEditorDialog.tsx`
> * `src/components/SavedPlaylistsDialog.tsx`
> * `src/components/SettingsPanel.tsx`
> * `src/components/StartScreen.tsx`
> * `src/components/StatsPanel.tsx`
> * `src/components/StatusBadge.tsx`
> * `src/components/StreamPlayer.tsx`
> * `src/components/ThumbnailPanel.tsx`
> * `src/components/Toolbar.tsx`
> * `src/hooks/useChromecast.ts`
> * `src/hooks/useMenuEventBridge.ts`
> * `src/hooks/usePlaylistSources.ts`
> * `src/hooks/useScan.helpers.ts`
> * `src/hooks/useScan.ts`
> * `src/hooks/useStreamPlayer.ts`
> * `src/hooks/useUpdateCheck.ts`
> * `src/index.css`
> * `src/lib/cast.ts`
> * `src/lib/channelResults.ts`
> * `src/lib/duplicates.ts`
> * `src/lib/errors.ts`
> * `src/lib/extinf.ts`
> * `src/lib/filters.ts`
> * `src/lib/format.ts`
> * `src/lib/haptics.ts`
> * `src/lib/languageDistribution.ts`
> * `src/lib/logger.ts`
> * `src/lib/logoCache.ts`
> * `src/lib/perf.ts`
> * `src/lib/playback.ts`
> * `src/lib/playlistReportVisibility.ts`
> * `src/lib/recentPlaylists.ts`
> * `src/lib/runtimeMonitor.ts`
> * `src/lib/savedPlaylists.ts`
> * `src/lib/scanState.ts`
> * `src/lib/shortcuts.ts`
> * `src/lib/sourceFilter.ts`
> * `src/lib/tableColumns.ts`
> * `src/lib/tauri.ts`
> * `src/lib/thumbnailState.ts`
> * `src/lib/types.ts`
> * `src/lib/updateState.ts`
> * `src/lib/workerSupport.ts`
> * `src/main.tsx`
> * `src/store/index.ts`
> * `src/store/slices/scanSlice.ts`
> * `src/store/slices/settingsSlice.ts`
> * `src/store/slices/uiSlice.ts`
> * `src/store/store.ts`
> * `src/store/types.ts`
> * `tests/channelResults.test.ts`
> * `tests/duplicates.test.ts`
> * `tests/errors.test.ts`
> * `tests/exportScope.test.ts`
> * `tests/filters.logic.test.ts`
> * `tests/format.test.ts`
> * `tests/playlistReportVisibility.test.ts`
> * `tests/savedPlaylists.test.ts`
> * `tests/scanErrorEvents.test.ts`
> * `tests/sourceFilter.test.ts`
> * `tests/tableColumns.test.ts`
> * `tests/updateState.test.ts`
> * `tests/useScan.helpers.test.ts`
> * `tests/useStreamPlayer.test.ts`
> 
> </details>
> 
> You can disable this status message by setting the `reviews.review_status` to `false` in the CodeRabbit configuration file.

<!-- end of auto-generated comment: rate limited by coderabbit.ai -->

<!-- finishing_touch_checkbox_start -->

<details>
<summary>✨ Finishing Touches</summary>

<details>
<summary>🧪 Generate unit tests (beta)</summary>

- [ ] <!-- {"checkboxId": "f47ac10b-58cc-4372-a567-0e02b2c3d479", "radioGroupId": "utg-output-choice-group-5074627217"} -->   Create PR with unit tests
- [ ] <!-- {"checkboxId": "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "radioGroupId": "utg-output-choice-group-5074627217"} -->   Commit unit tests in branch `feat/auto-updater-and-quality-gates`

</details>

</details>

<!-- finishing_touch_checkbox_end -->
<!-- tips_start -->

---

Thanks for using [CodeRabbit](https://coderabbit.ai?utm_source=oss&utm_medium=github&utm_campaign=kristofferR/IPTVChecker&utm_content=214)! It's free for OSS, and your support helps us grow. If you like it, consider giving us a shout-out.

<details>
<summary>❤️ Share</summary>

- [X](https://twitter.com/intent/tweet?text=I%20just%20used%20%40coderabbitai%20for%20my%20code%20review%2C%20and%20it%27s%20fantastic%21%20It%27s%20free%20for%20OSS%20and%20offers%20a%20free%20trial%20for%20the%20proprietary%20code.%20Check%20it%20out%3A&url=https%3A//coderabbit.ai)
- [Mastodon](https://mastodon.social/share?text=I%20just%20used%20%40coderabbitai%20for%20my%20code%20review%2C%20and%20it%27s%20fantastic%21%20It%27s%20free%20for%20OSS%20and%20offers%20a%20free%20trial%20for%20the%20proprietary%20code.%20Check%20it%20out%3A%20https%3A%2F%2Fcoderabbit.ai)
- [Reddit](https://www.reddit.com/submit?title=Great%20tool%20for%20code%20review%20-%20CodeRabbit&text=I%20just%20used%20CodeRabbit%20for%20my%20code%20review%2C%20and%20it%27s%20fantastic%21%20It%27s%20free%20for%20OSS%20and%20offers%20a%20free%20trial%20for%20proprietary%20code.%20Check%20it%20out%3A%20https%3A//coderabbit.ai)
- [LinkedIn](https://www.linkedin.com/sharing/share-offsite/?url=https%3A%2F%2Fcoderabbit.ai&mini=true&title=Great%20tool%20for%20code%20review%20-%20CodeRabbit&summary=I%20just%20used%20CodeRabbit%20for%20my%20code%20review%2C%20and%20it%27s%20fantastic%21%20It%27s%20free%20for%20OSS%20and%20offers%20a%20free%20trial%20for%20proprietary%20code)

</details>


<sub>Comment `@coderabbitai help` to get the list of available commands.</sub>

<!-- tips_end -->
