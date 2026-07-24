<!-- MURMUR_IGNORE -->
🟠 **High** `Layers/ProjectionPipeline.ts:1554`

During bootstrap, the `threadMessages` projector runs before the `queuedMessages` projector. When the event stream contains a queued message followed by `thread.reverted`, the `threadMessages` projector's revert handler queries `projectionQueuedMessageRepository.listByThreadId` — but that projection table is still empty because `queuedMessages` hasn't replayed yet. As a result, `collectThreadAttachmentRelativePaths` omits queued-message attachments from the retained set, and the post-transaction prune deletes their files. The queued projector then restores only the database rows, leaving queued messages pointing at attachment files that are permanently missing after a restart or rebuild. Reorder the `projectors` array so `queuedMessages` runs before `threadMessages`, or derive retained queued attachments without depending on another projector's bootstrap state.

✅ Resolved in 148c355df49cc1692434f4d6689f53666523cadc