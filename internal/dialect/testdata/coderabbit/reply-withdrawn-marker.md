`@kristofferR`, you’re right—thank you for the precise correction. I conflated a GitHub-level conclusion with the bot-specific meaning of that conclusion.

`startup_failure` is already handled centrally, while `skipped` must remain classified by each bot: Bugbot treats it as non-delivery, but Macroscope’s “No code objects were reviewed” is valid clean-review evidence. I have no contrary Macroscope capture, so this finding should be withdrawn.

🐇 thoughtful rabbit nod

<!-- <review_comment_withdrawn> -->

---

<details>
<summary>✏️ Learnings added</summary>

```
Learnt from: kristofferR
Repo: kristofferR/coderabbit-queue PR: 35
File: internal/dialect/coreviewer.go:16-21
Timestamp: 2026-07-24T23:19:08.208Z
Learning: In the Go co-reviewer dialect registry, `internal/dialect/coreviewer.go` deliberately does not classify the GitHub Check Run conclusion `skipped` in shared `checkRunFailed`: delivery semantics are bot-specific. `ClassifyBugbotCheck` treats `skipped` as `CheckFailed`, whereas Macroscope Correctness can treat a `skipped` check with “No code objects were reviewed” as `CheckDoneClean`. Captured corpus fixtures and `TestCheckRunsThatAreNotReviewEvidence` define these semantics.
```

> Note: Learnings are effective only in the context of similar code segments. To apply general coding guidelines, please configure [review instructions](https://docs.coderabbit.ai/guides/review-instructions). You can manage existing learnings in the [app](https://app.coderabbit.ai/learnings).

</details>



<!-- This is an auto-generated reply by CodeRabbit -->
