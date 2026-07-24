🟡 **Medium** `chat/MarkdownMedia.tsx:10`

`DIRECT_MEDIA_SRC_PATTERN` omits protocol-relative URLs, so a markdown image source like `//cdn.example.com/image.png` is misclassified as a workspace path instead of being loaded directly by the browser. The code then requests a signed workspace asset for a non-existent local file, the lookup fails, and the image renders as "Media unavailable" instead of displaying the remote image. Consider adding `//` to the pattern so protocol-relative URLs are treated as direct media sources.

```suggestion
+/** Sources the browser can load directly without a signed workspace asset URL. */
+const DIRECT_MEDIA_SRC_PATTERN = /^(?:https?:|data:|blob:|\/\/)/i;
```

<details>
<summary>Also found in 1 other location(s)</summary>

`apps/web/src/components/ChatMarkdown.tsx:1498`

> The new `img`/`video` renderers route every source through `MarkdownMedia`, whose direct-media check only recognizes strings beginning with `http:`, `https:`, `data:`, or `blob:`. Valid protocol-relative external media URLs such as `![image](//cdn.example.com/a.png)` are therefore misclassified as workspace files (or immediately shown as unavailable when `threadRef` is absent), so media that previously rendered normally no longer loads.


</details>



<details>
<summary>🚀 Reply "<strong>fix it for me</strong>" or copy this <strong>AI Prompt</strong> for your agent:</summary>

```text
In file @apps/web/src/components/chat/MarkdownMedia.tsx around line 10:

`DIRECT_MEDIA_SRC_PATTERN` omits protocol-relative URLs, so a markdown image source like `//cdn.example.com/image.png` is misclassified as a workspace path instead of being loaded directly by the browser. The code then requests a signed workspace asset for a non-existent local file, the lookup fails, and the image renders as "Media unavailable" instead of displaying the remote image. Consider adding `//` to the pattern so protocol-relative URLs are treated as direct media sources.

Also found in 1 other location(s):
- apps/web/src/components/ChatMarkdown.tsx:1498 -- The new `img`/`video` renderers route every source through `MarkdownMedia`, whose direct-media check only recognizes strings beginning with `http:`, `https:`, `data:`, or `blob:`. Valid protocol-relative external media URLs such as `![image](//cdn.example.com/a.png)` are therefore misclassified as workspace files (or immediately shown as unavailable when `threadRef` is absent), so media that previously rendered normally no longer loads.

```
</details>