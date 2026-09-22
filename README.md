# ppars

> Extract parameter names from a list of URLs.

`ppars` is a small, fast, single-binary utility that reads URLs (or any
text that contains URLs) and pulls out every parameter name it can find.
The result is a clean, deduplicated wordlist that you can feed straight
into a fuzzer, an HTTP client, or another recon step.

It is designed for the reconnaissance phase of web application security
testing — the moment when you have thousands of URLs collected by a
crawler or a proxy but need to know which parameters actually appear in
them.

---

## Table of contents

- [Why](#why)
- [Features](#features)
- [Installation](#installation)
- [Quick start](#quick-start)
- [Usage](#usage)
- [How extraction works](#how-extraction-works)
- [Examples](#examples)
- [Input format](#input-format)
- [Output format](#output-format)
- [Performance](#performance)
- [Contributing](#contributing)
- [License](#license)

---

## Why

A single HTTP response often leaks parameter names in many places: URLs,
redirect `Location` headers, JSON bodies, HTML `href` attributes, inline
JavaScript, and log lines. Collecting them manually does not scale past a
few hundred entries, and a naive `key=` grep on real-world input produces
far more noise than signal — minified JavaScript, base64 blobs, and
percent-encoded JSON all contain `=` characters that are not parameters.

`ppars` solves this with a **layered extraction strategy**: it prefers
URL-aware parsing, falls back to separator-based regexes, and only uses a
permissive `name=` matcher when nothing else has matched. The result is a
high-signal wordlist without the noise.

Typical pipeline usage:

```bash
katana -u https://target.example -silent | ppars -
cat proxy-history.txt | ppars -o params.txt
ppars urls.txt -v -sort
```

---

## Features

- **Layered extraction** — three tiers, each invoked only when the
  previous tier produced no result (see
  [How extraction works](#how-extraction-works)).
- **URL-aware** — the first tier uses proper URL parsing, so
  percent-encoded and repeated keys are handled correctly.
- **Path and fragment parameters** — catches `/api/v1/users?name=…` and
  `#token=…`, not just query strings.
- **Strict key validation** — rejects keys that are obviously not
  parameters (base64 blobs, minified identifiers, pure numbers), which
  is what keeps the output usable on noisy input.
- **Concurrent** — configurable worker pool; saturates all cores on
  multi-million-line inputs.
- **Deduplicated** — twice (locally per worker, then globally).
- **Streaming-friendly** — reads from a file or stdin, writes to a file
  or stdout, and can be dropped into any Unix pipeline.
- **Deterministic on demand** — `-sort` gives stable, diffable output.
- **Interrupt-safe** — Ctrl-C drains the queue and flushes results
  instead of throwing away the whole run.
- **Zero dependencies** — pure Go standard library; a single static
  binary.

---

## Installation

### Build from source (recommended)

```bash
git clone https://github.com/<you>/ppars.git
cd ppars
go build -o ppars main.go
```

The result is a single self-contained binary that can be copied to any
machine with the same OS and CPU architecture.

### Cross-compilation

Because `ppars` has no external dependencies, cross-compiling is a
one-liner:

```bash
# Linux amd64
GOOS=linux GOARCH=amd64 go build -o ppars-linux-amd64 main.go

# macOS arm64 (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o ppars-darwin-arm64 main.go

# Windows amd64
GOOS=windows GOARCH=amd64 go build -o ppars-windows-amd64.exe main.go
```

### Requirements

- Go **1.20+** to build.
- No runtime dependencies.

---

## Quick start

```bash
# Extract from a file. Output is written to ./urls__p.txt.
ppars urls.txt

# Extract from a pipe. Output is written to stdout.
cat urls.txt | ppars -

# Extract, sort, and show progress. Output to a custom file.
ppars urls.txt -o params.txt -sort -v
```

---

## Usage

```
ppars [options] <input-file>
cat urls.txt | ppars [options]
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-c N` | `20` | Number of concurrent workers. |
| `-o PATH` | *(auto)* | Output file. Use `-` for stdout. |
| `-v` | off | Print progress and a summary to stderr. |
| `-sort` | off | Sort output alphabetically before writing. Buffers all results in memory. |
| `-version` | — | Print version and exit. |
| `-h`, `-help` | — | Print usage. |

### Input modes

`ppars` supports two input modes:

1. **File** — pass a path as the first positional argument.
2. **Stdin** — omit the positional argument and pipe input into the
   process. If stdin is a terminal (i.e. nothing is piped in), the usage
   message is printed and the process exits.

### Output modes

The output destination is resolved in this order:

1. `-o -` → stdout (explicit).
2. `-o FILE` → that file.
3. File input and no `-o` → auto-derived name **next to the input file**.
4. Stdin input and no `-o` → stdout.

The auto-derived name replaces any trailing `__tag` in the input
basename with `__p`:

| Input | Auto output |
|-------|-------------|
| `urls.txt` | `urls__p.txt` |
| `urls__live.txt` | `urls__p.txt` |
| `u__prod.txt` | `u__p.txt` |
| `/tmp/lists/urls.txt` | `/tmp/lists/urls__p.txt` |

This makes the output path stable across renames of the input, which is
useful when you keep several tagged variants of the same list.

---

## How extraction works

Each line is processed by three extraction tiers, and **each tier only
runs if the previous tiers produced no result**. This ordering is what
keeps the output clean: the later tiers are more permissive and therefore
noisier, so we want to reach them only when the cheaper, more precise
tiers have already failed.

### Tier 1 — URL parsing

The line is parsed with Go's `net/url`. If it is a well-formed absolute
URL, the query string is decomposed and every key is emitted. This tier
handles percent-encoding correctly and is by far the most accurate.

Example: `https://x.com/a?user_id=1&token=abc`
→ `user_id`, `token`

### Tier 2 — separator-based regexes

Three regexes are applied to the whole line:

| Pattern | Matches |
|---------|---------|
| `[?&;]([^&=]+)=` | query-string parameters after `?`, `&`, or `;` |
| `/([^/?#]+)=` | path parameters after a `/` |
| `#([^&=]+)=` | fragment parameters after `#` |

This tier catches relative URLs, path parameters, and fragments that
`url.Parse` cannot decompose on its own.

Examples:
- `/api/v1/users?name=admin` → `name`
- `/path;jsessionid=abc/next` → `jsessionid`
- `#access_token=xyz` → `access_token`

### Tier 3 — permissive fallback

If neither tier 1 nor tier 2 produced anything, the line is scanned with
a permissive `[a-zA-Z_][a-zA-Z0-9_-]*=` matcher. This tier is what picks
up parameter names from free-form text — log lines, code snippets, HTML
attributes, scraped data.

It is deliberately not run unless the earlier tiers fail, because on
ordinary input it would match almost anything containing an `=`.

### Key validation

Every candidate name, regardless of which tier produced it, must pass the
same validation rule before being emitted:

```
^[a-zA-Z_][a-zA-Z0-9_-]{0,29}(?:\[\])?$
```

This accepts names like `user_id`, `_csrf`, `filter[]`, and rejects
names that are obviously not parameters: pure numbers, `===`, base64
fragments, and minified identifiers. The 30-character limit is generous
enough for virtually all real parameters and short enough to filter out
most junk.

---

## Examples

### 1. Basic extraction from a file

```bash
ppars urls.txt
```

Writes `urls__p.txt` next to `urls.txt`.

### 2. Extract from a pipe, keep stdout clean for the next tool

```bash
katana -u https://target.example -silent | ppars - | sort -u
```

### 3. Sort output and show progress

```bash
ppars urls.txt -sort -v -o params.txt
```

Progress lines go to stderr; the parameter list goes to `params.txt`.

### 4. Reuse as a stage in a larger pipeline

```bash
cat access.log \
    | grep -oE 'https?://[^ ]+' \
    | ppars - -sort \
    | while read p; do
          ffuf -u "https://target.example/?${p}=FUZZ" -w wordlist.txt
      done
```

### 5. Reduce concurrency on a low-power machine

```bash
ppars huge-list.txt -c 2
```

### 6. Print version

```bash
ppars -version
```

---

## Input format

`ppars` reads **one URL per line** from the input.

- Leading and trailing whitespace on each line is trimmed.
- Empty lines are skipped.
- Line length is capped at **10 MiB** to prevent a single pathological
  line from exhausting memory. Inputs larger than this are almost
  certainly garbage or a denial-of-service attempt.
- Encoding is assumed to be UTF-8; lines are treated as byte strings
  otherwise, so any ASCII-compatible encoding will work.

`ppars` does not attempt to parse free-form text into URLs on its own.
If your input is a raw log file or an HTML dump, run it through a URL
extractor first (e.g. `grep -oE 'https?://[^ ]+'`) so that each line
contains a single URL.

---

## Output format

The output is a plain text file with **one parameter name per line**, no
quotes, no escaping.

- **Duplicates are removed.** A parameter appearing in a thousand URLs is
  emitted once.
- **Order is not preserved by default.** Workers run in parallel, so the
  output order depends on scheduling. Use `-sort` for a deterministic
  order.
- **Only the name is emitted**, not the value. Values are highly
  instance-specific and rarely useful as a wordlist.

Example:

```
access_token
filter[]
id
name
page
q
user_id
```

---

## Performance

`ppars` is designed for inputs of millions of lines. Approximate
throughput on a modern x86-64 laptop, with 20 workers:

| Input size | Time |
|------------|------|
| 10,000 URLs | < 50 ms |
| 1,000,000 URLs | ~ 1–2 s |
| 10,000,000 URLs | ~ 15–25 s |

Memory use is bounded by:

- the size of the deduplication sets, which is O(unique parameters) —
  typically a few thousand entries, tens of KB;
- the fixed channel buffers (10,000 entries each).

`-sort` adds a buffer holding every unique parameter before writing, but
because the unique set is usually small this is rarely a concern.

If you need to process an input larger than available memory, `-sort` is
the only feature that has any scaling constraint; the default streaming
mode is bounded regardless of input size.

---

## Contributing

Contributions are welcome. Before opening a PR:

1. Ensure `go vet ./...` is clean.
2. Ensure `gofmt -l .` reports nothing.
3. Add a test case for any new extraction rule in `parser_test.go`.
4. Keep the diff focused — one feature or fix per PR.

If you are adding a new extraction tier or pattern, please include a
minimal example in `testdata/` so the behavior is regression-tested.

---

## License

MIT. See [`LICENSE`](LICENSE) for details.
