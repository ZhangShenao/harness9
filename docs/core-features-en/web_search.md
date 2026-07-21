# Web Search and Fetch Technical Design

harness9 grants Agents real-time internet access through two built-in tools, `web_search` and `web_fetch`, requiring no API Key and shipping with production-grade SSRF protection.

---

## 1. Design Decisions

### Why not build this as an MCP tool

DeepAgents and OpenCode fully outsource web capability to MCP servers, meaning zero configuration equals zero capability — users must install and configure a Tavily or Brave MCP themselves. harness9's philosophy is **out-of-the-box readiness**: DuckDuckGo requires no Key at all, `go-readability` is a pure Go library, and the entire capability chain depends on no external platform account.

### Tools stay atomic, deep search lives at the LLM layer

The tools provide only two atomic capabilities: search (return result links) and fetch (return page content). Whether to keep following links, whether to expand the search terms, when to stop — these decisions are entirely delegated to the LLM within the ReAct loop. This is the shared design choice across six mainstream frameworks; the tool layer neither needs nor should embed search-tree traversal logic.

### Why inject the current date

There is a gap between the LLM's knowledge cutoff (training cutoff) and the actual current time. Without date injection, an Agent tends to use the training cutoff year when searching, causing it to retrieve stale results when searching for "the latest version." harness9 injects the real-time date via `time.Now().Format("2006-01-02")` into the base section of the System Prompt, so the Agent automatically perceives the current date in every session with no extra tool call needed.

---

## 2. Tool Interface

### `web_search`

Searches the internet and returns a list of titles, URLs, and snippets.

| Parameter | Type | Required | Default | Description |
|------|------|:----:|--------|------|
| `query` | string | Yes | — | Search query; English yields better results |
| `max_results` | int | No | 5 | Number of results returned, range 1–10 |

**Output format:**

```
[1] Go 1.25 Release Notes
URL: https://go.dev/doc/go1.25
摘要: Go 1.25 introduces range-over-func, improved type inference...

[2] What's new in Go 1.25
URL: https://blog.golang.org/go1.25
摘要: The Go team is pleased to announce Go 1.25...
```

### `web_fetch`

Fetches the given URL and returns the main content in Markdown format.

| Parameter | Type | Required | Default | Description |
|------|------|:----:|--------|------|
| `url` | string | Yes | — | Target URL (http/https) |
| `max_chars` | int | No | 8000 | Maximum characters returned, capped at 32000 |

**Output format:**

```markdown
# Go 1.25 Release Notes

> 来源：https://go.dev/doc/go1.25

## New Features

...main content (Markdown format)...

[内容已截断，已显示前 8000 字符]   ← only appears when the limit is exceeded
```

---

## 3. SSRF Protection (`web_safety.go`)

Every HTTP request is validated by `isSafeURL` before being sent, analogous to the sandbox protection `safe_path.go` provides for filesystem paths.

### Check chain

```
URL input
  │
  ▼
① scheme validation: must be http or https, reject ftp / file etc.
  │
  ▼
② userinfo rejection: any URL containing user:pass@... format is rejected
  │
  ▼
③ DNS resolution: net.LookupHost(hostname)
  │
  ├── resolution failure → fail-closed (reject the request)
  │              the first line of defense against DNS rebinding attacks
  │
  ▼
④ IP range check: check every resolved IP address
```

### Permanently blocked IP ranges

| CIDR | Description | Configurable |
|------|------|:------:|
| `169.254.0.0/16` | Link-local + AWS/Azure/GCP metadata endpoint | Permanently blocked |
| `127.0.0.0/8` | IPv4 loopback | Blocked by default |
| `::1` | IPv6 loopback (independently checked via `net.IPv6loopback`) | Blocked by default |
| `10.0.0.0/8` | RFC1918 private network | Blocked by default |
| `172.16.0.0/12` | RFC1918 private network | Blocked by default |
| `192.168.0.0/16` | RFC1918 private network | Blocked by default |
| `100.64.0.0/10` | CGNAT | Blocked by default |
| `fe80::/10` | IPv6 link-local (equivalent to IPv4 169.254.0.0/16) | Blocked by default |
| `fc00::/7` | IPv6 Unique Local Address (ULA) (equivalent to IPv4 RFC1918 private network) | Blocked by default |

### Why re-check the IP after DNS resolution

An attacker can have a domain resolve first to a public IP to pass validation, then switch the resolution to an internal address within the DNS TTL window (DNS rebinding). Resolving first and then checking the IP cuts off this path — the IP `isSafeURL` receives is the real IP at the moment of the DNS query, not the hostname.

### IPv4-mapped IPv6 normalization

`::ffff:169.254.169.254` is the IPv6 representation of `169.254.169.254`. The code normalizes IPv4-mapped IPv6 addresses to IPv4 via `ip.To4()`, ensuring internal addresses expressed in IPv6 form cannot bypass the CIDR check.

### Redirect chain SSRF protection

`web_fetch`'s `CheckRedirect` callback re-invokes `isSafeURL` on the new URL after every redirect, preventing an open-redirect → internal-network attack path:

```
External URL → SSRF check ✓ → server 301 → internal URL → SSRF check ✗ (rejected)
```

---

## 4. HTML Content Extraction Pipeline (`web_content.go`)

After `web_fetch` retrieves HTML, it goes through a three-stage pipeline that converts it into LLM-friendly Markdown:

```
HTML body (io.Reader)
    │
    ▼
① Size limit (1MB)
    │ exceeded → return error
    ▼
② go-readability.FromReader()
    │ success → Article{Title, Content(HTML)}
    │ failure → go straight to fallback
    ▼
③ html-to-markdown.Convert(Article.Content)
    │ success → Markdown string
    │ failure → use Article.TextContent (plain text extracted by readability)
    ▼
④ assemblePage(rawURL, title, content, maxChars)
    │ assemble output: # title + > source + content
    │ exceeds maxChars → truncate + append marker
    ▼
Markdown output
```

**Fallback path**: when `go-readability` fails entirely (e.g. minimal HTML or empty content), a simple text extraction is done using the `golang.org/x/net/html` tokenizer — skipping the contents of `<script>` and `<style>` tags and concatenating visible text nodes. This tokenizer is an indirect dependency within the Go standard library's scope, adding zero extra dependencies.

### Size constants

| Constant | Value | Description |
|------|-----|------|
| `defaultMaxChars` | 8,000 | Default value of the `max_chars` parameter (~2000 tokens) |
| `hardMaxChars` | 32,000 | Upper bound of the `max_chars` parameter (~8000 tokens) |
| `maxHTMLBodySize` | 1,048,576 (1MB) | Upper limit for reading the HTTP response body |

---

## 5. DuckDuckGo Search Backend (`web_search.go`)

### Why DuckDuckGo was chosen

After surveying the search backend choices of six mainstream frameworks: DuckDuckGo is the only backend **supported by every framework with no API Key requirement**, and its HTML endpoint (`html.duckduckgo.com/html/`) returns plain HTML — no JavaScript rendering is needed, and a standard HTTP client can handle it directly.

### Request flow

```
POST https://html.duckduckgo.com/html/
Content-Type: application/x-www-form-urlencoded
User-Agent: harness9/1.0
Body: q=<url-encoded-query>
```

Timeout controls:
- **Dial timeout**: 10s (TCP handshake phase, `net.Dialer.Timeout`)
- **Request timeout**: 20s (Context deadline, covering DNS + dial + response read)

The dual timeout addresses a known issue with `http.DefaultClient`: when only a context timeout is used, an extremely slow TCP handshake may not be interrupted promptly; an independent dial timeout guarantees responsiveness during the connection-establishment phase.

### HTML parsing

`golang.org/x/net/html` (an existing indirect dependency, zero new additions) builds a full DOM tree via `html.Parse()`, recursively traversed to extract:

| Selector | Target content |
|--------|---------|
| `div.result` (excluding `div.result--more`) | Container for a single search result |
| `a.result__a` | Title text + href |
| `a.result__snippet` | Snippet text |

### DDG redirect URL decoding

DuckDuckGo's links use a redirect format:

```
href="/l/?uddg=https%3A%2F%2Fexample.com%2Fpath&rut=..."
```

`decodeUDDG()` extracts the real target URL from the `uddg` parameter, so the LLM receives the final URL that can be passed directly into `web_fetch`.

### Testability design

The `backendURL` field of the `WebSearchTool` struct points to the DDG endpoint by default, and can be replaced with a local `httptest.NewServer` address in tests, requiring no real network requests:

```go
// production
tool := NewWebSearchTool()  // backendURL = "https://html.duckduckgo.com/html/"

// testing
tool := &WebSearchTool{backendURL: server.URL}
```

Likewise, `WebFetchTool`'s `safetyCheck` field is replaced with a no-op in tests, allowing access to a 127.0.0.1 httptest server without triggering SSRF blocking.

---

## 6. Current Date Injection (`internal/context/builder.go`)

`DefaultPromptBuilder.Build()` injects today's date into the base System Prompt:

```
工作目录：/path/to/project
当前日期：2026-06-12
```

Every `Build()` call executes `time.Now().Format("2006-01-02")` in real time, ensuring the Agent perceives the correct date in every session, so searches naturally carry the current year and avoid stale search results caused by training-cutoff drift.

---

## 7. Module Structure

```
internal/tools/
├── web_safety.go        # SSRF protection: isSafeURL (check chain + blockedCIDRs)
├── web_safety_test.go   # 19 test cases: IP ranges / scheme / DNS fail-closed / IPv6 / IPv6 ULA / IPv6 link-local
├── web_content.go       # HTML→Markdown pipeline: extractContent + extractPlainText + assemblePage (UTF-8-safe truncation)
├── web_content_test.go  # 5 test cases: basic conversion / truncation / fallback / oversized body / UTF-8-safe truncation
├── web_fetch.go         # WebFetchTool: HTTP GET + Content-Type branching
├── web_fetch_test.go    # 6 test cases: SSRF / HTML / text / unsupported / empty URL / 4xx
├── web_search.go        # WebSearchTool: DuckDuckGo POST + DOM parsing + decodeUDDG
└── web_search_test.go   # 6 test cases: DDG parsing / max_results / URL decoding / Execute / empty results
```

**Dependency relationships:**

```
web_fetch.go ──────┐
                   ├──→ web_safety.go (isSafeURL)
web_search.go ─────┘
     │
     └──→ web_content.go (extractContent)  ──→ go-readability
                                           ──→ html-to-markdown
                                           ──→ golang.org/x/net/html (existing dependency)
```

---

## 8. Configuration and Usage

Both tools are automatically registered when harness9 starts, and can be used by the main agent and all Sub-Agents with zero configuration required.

**Typical search flow:**

```
User: search for Go 1.25's new features

Agent:
  1. web_search("Go 1.25 release notes") → get the result list
  2. web_fetch("https://go.dev/doc/go1.25") → get detailed content
  3. answer the user based on the fetched content
```

**Sub-Agent delegation (research scenario):**

```
@general-purpose search for LangGraph's latest checkpoint mechanism and summarize the key APIs
```

The `general-purpose` Sub-Agent inherits all of the main agent's tools, including `web_search` and `web_fetch`, and can independently complete multi-round search + fetch + summarize, reporting back only the conclusion to the main context.

---

## 9. Limitations and Caveats

- **DuckDuckGo rate limiting**: no official SLA; high-frequency calls may trigger temporary blocking. For stable, high-frequency search, consider configuring Brave Search or Tavily via environment variables (pending P2 backend interface implementation)
- **JavaScript-rendered pages**: `web_fetch` uses plain HTTP requests and does not execute JavaScript, so dynamically rendered SPA content cannot be fetched
- **`go-readability` deprecation notice**: currently using `github.com/go-shiori/go-readability`, which the author has marked deprecated; future migration to `codeberg.org/readeck/go-readability/v2` is recommended
- **1MB size limit**: pages larger than 1MB are rejected with an error

---

## 10. Reference Implementations

| Framework | Strategy | Key characteristics |
|------|------|---------|
| OpenHarness | Minimal built-in | DuckDuckGo HTML parsing + self-implemented HTML→plain text, 12000-character truncation |
| OpenClaw | Pluggable hybrid | Mozilla Readability + htmlToMarkdown, 5+ search backends with cascading fallback |
| HermesAgent | Full-featured | 8 backends, LLM content compaction pipeline (Gemini Flash, supports 2M-character chunking) |
| Claude Agent SDK | Officially built-in | Native `WebSearch`/`WebFetch` tools; research-agent example demonstrates Sub-Agent parallel research pattern |

For detailed research, see `docs/技术调研/web-search-capability.md` (local research document, not published with the repository).
