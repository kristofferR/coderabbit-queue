<!-- MURMUR_IGNORE -->
#### Approvability

**Verdict:** Approved

This is a targeted bug fix for thread settling logic on merged/closed PRs. The change adds an idle guard to prevent threads from immediately re-settling after follow-up activity. The author owns this code, the scope is limited, and comprehensive tests are included.

<sup>You can customize Macroscope's approvability policy. [Learn more](https://docs.macroscope.com/approvability#example-custom-eligibility-rules).</sup>