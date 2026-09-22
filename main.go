package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// =============================================================================
// Constants
// =============================================================================

const (
	version  = "0.2.0"
	progName = "ppars"

	// Channel buffer size for both the URL and parameter channels.
	// Large enough to absorb short bursts from a fast producer,
	// small enough that memory stays predictable on huge inputs.
	channelBuffer = 10_000

	// Maximum length of a single input line. Anything larger is almost
	// certainly garbage or a memory-exhaustion attempt, so we cap it to
	// prevent a single pathological line from blowing up the process.
	maxLineBytes = 10 * 1024 * 1024

	// How often (in URLs read) to print a progress line when -v is set.
	// Large enough to avoid spamming stderr on big files.
	progressEvery = 100_000
)

// =============================================================================
// Regex patterns
// =============================================================================

// validKeyRE matches acceptable parameter names.
//
// It is intentionally strict. Real application keys look like `user_id`,
// `filter[]`, or `_csrf`; they do not look like `=`, `123abc=`, or
// `some.dotted.token=`. Filtering here prevents a huge amount of noise
// from URLs that contain base64 blobs, encoded JSON, or minified code.
var validKeyRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]{0,29}(?:\[\])?$`)

// fallbackRE matches any bare `identifier=`. It is used only as a last
// resort — after the URL parser and every primary pattern have failed —
// so that ordinary text does not drown the output in false positives.
var fallbackRE = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_-]*)=`)

// defaultPrimaryPatterns are applied to every input line. Each pattern
// must capture the parameter name in its first group; any remaining
// groups are ignored.
//
// The three patterns cover the three places a parameter can appear:
//
//   * after a `?`, `&`, or `;` (query string)
//   * inside the path after a `/` (path parameter)
//   * after a `#` (fragment parameter)
var defaultPrimaryPatterns = []string{
	`[?&;]([^&=]+)=`,
	`/([^/?#]+)=`,
	`#([^&=]+)=`,
}

// =============================================================================
// Parser
// =============================================================================

// Parser extracts parameter names from raw URL strings.
type Parser struct {
	primary  []*regexp.Regexp
	fallback *regexp.Regexp
	validKey *regexp.Regexp
}

// New returns a Parser configured with the given primary regexes.
//
// If patterns is empty, the built-in defaults are used. Invalid patterns
// are reported on stderr and skipped; the resulting Parser is still
// usable and will simply lack the corresponding extraction rule.
func New(patterns []string) *Parser {
	if len(patterns) == 0 {
		patterns = defaultPrimaryPatterns
	}

	p := &Parser{
		fallback: fallbackRE,
		validKey: validKeyRE,
	}

	for _, pat := range patterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[!] ignoring invalid regex %q: %v\n", pat, err)
			continue
		}
		if re.NumSubexp() < 1 {
			fmt.Fprintf(os.Stderr, "[!] ignoring regex %q: no capture group\n", pat)
			continue
		}
		p.primary = append(p.primary, re)
	}

	return p
}

// Extract returns the parameter names present in raw.
//
// Extraction is layered, and each tier runs only when the previous tier
// could not handle the input. This ordering matters: the later tiers are
// more permissive and therefore noisier, so we want to reach them only
// when the cheaper, more precise tiers have already failed.
//
//	Tier 1 — url.Parse
//	    Reliable for well-formed absolute URLs. Gives us the
//	    query-string keys directly, with correct handling of
//	    percent-encoding and repeated keys.
//
//	Tier 2 — primary regexes
//	    Handles relative URLs, path parameters, and fragment
//	    parameters that url.Parse does not decompose. Each regex is
//	    expected to capture the name in its first group.
//
//	Tier 3 — fallback regex
//	    A permissive `name=` matcher, run only when neither of the two
//	    tiers above produced a hit. This is what catches parameters in
//	    free-form input (log lines, code snippets, scraped HTML).
func (p *Parser) Extract(raw string) []string {
	var params []string
	found := false

	// Tier 1: proper URL parsing.
	if u, err := url.Parse(raw); err == nil && u.RawQuery != "" {
		for key := range u.Query() {
			if p.validKey.MatchString(key) {
				params = append(params, key)
				found = true
			}
		}
	}

	// Tier 2: separator-based regexes.
	for _, re := range p.primary {
		for _, m := range re.FindAllStringSubmatch(raw, -1) {
			if len(m) < 2 {
				continue
			}
			if key := m[1]; p.validKey.MatchString(key) {
				params = append(params, key)
				found = true
			}
		}
	}

	// Tier 3: permissive fallback. Only reached when tiers 1 and 2
	// produced nothing, because otherwise its high false-positive rate
	// would pollute the output.
	if !found {
		for _, m := range p.fallback.FindAllStringSubmatch(raw, -1) {
			if len(m) < 2 {
				continue
			}
			if key := m[1]; p.validKey.MatchString(key) {
				params = append(params, key)
			}
		}
	}

	return params
}

// =============================================================================
// Output naming
// =============================================================================

// autoOutputName derives the default output path from the input path.
//
// Any trailing `__tag` in the input basename is stripped and replaced
// with `__p` (for "parameters"), so:
//
//	targets.txt        -> targets__p.txt
//	targets__live.txt  -> targets__p.txt
//	u__prod.txt        -> u__p.txt
//
// The goal is a stable output path that survives input renames. Users
// routinely keep several tagged variants of the same list (live, prod,
// old, filtered) and expect the extracted parameter file to be shared,
// not duplicated per tag.
//
// The output is written next to the input file, not into the current
// working directory, because that matches the user's mental model:
// `ppars /path/to/list.txt` should keep the result alongside the source.
func autoOutputName(inputPath string) string {
	dir := filepath.Dir(inputPath)
	base := filepath.Base(inputPath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)

	if i := strings.LastIndex(name, "__"); i != -1 {
		name = name[:i]
	}

	return filepath.Join(dir, name+"__p"+ext)
}

// =============================================================================
// CLI
// =============================================================================

func buildFlagSet() (*flag.FlagSet, *int, *string, *bool, *bool, *bool) {
	workers := flag.Int("c", 20, "number of concurrent workers")
	outputPath := flag.String("o", "",
		"output file (default: auto-derived for file input, stdout for stdin; use '-' for stdout)")
	verbose := flag.Bool("v", false, "print progress to stderr")
	doSort := flag.Bool("sort", false,
		"sort output alphabetically (buffers all results in memory)")
	showVersion := flag.Bool("version", false, "print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "%s %s — extract parameter names from a list of URLs\n\n",
			progName, version)
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s [options] <input-file>\n", progName)
		fmt.Fprintf(os.Stderr, "  cat urls.txt | %s [options]\n\n", progName)
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}

	return nil, workers, outputPath, verbose, doSort, showVersion
}

// =============================================================================
// Entry point
// =============================================================================

func main() {
	_, workers, outputPath, verbose, doSort, showVersion := buildFlagSet()
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if *workers < 1 {
		fmt.Fprintln(os.Stderr, "[!] -c must be >= 1")
		os.Exit(1)
	}

	args := flag.Args()
	if len(args) > 1 {
		fmt.Fprintf(os.Stderr, "[!] ignoring extra arguments: %v\n", args[1:])
	}
	inputPath := ""
	if len(args) > 0 {
		inputPath = args[0]
	}

	// ----- Input -----------------------------------------------------------
	//
	// Two modes are supported, and we detect between them explicitly
	// rather than relying on ambiguous "is stdin a pipe?" checks:
	//
	//   * A positional argument means "read this file".
	//   * Otherwise, if stdin is a terminal, we print usage; if it is a
	//     pipe, we read from it.
	var (
		inputScanner *bufio.Scanner
		inputFile    *os.File
	)

	if inputPath != "" {
		f, err := os.Open(inputPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[!] failed to open input file: %v\n", err)
			os.Exit(1)
		}
		inputFile = f
		defer inputFile.Close()
		inputScanner = bufio.NewScanner(f)
		fmt.Fprintf(os.Stderr, "[+] Reading from: %s\n", inputPath)
	} else {
		stat, _ := os.Stdin.Stat()
		if stat != nil && (stat.Mode()&os.ModeCharDevice) != 0 {
			flag.Usage()
			os.Exit(1)
		}
		inputScanner = bufio.NewScanner(os.Stdin)
	}

	// ----- Output ----------------------------------------------------------
	//
	// Resolution order for the destination:
	//
	//   1. -o -       → stdout (explicit)
	//   2. -o FILE    → that file
	//   3. file input → auto-derived name next to the input
	//   4. stdin      → stdout
	var (
		output       io.Writer = os.Stdout
		outputCloser io.Closer
	)

	switch {
	case *outputPath == "-":
		// Already stdout.

	case *outputPath != "":
		f, err := os.Create(*outputPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[!] failed to create output file: %v\n", err)
			os.Exit(1)
		}
		output = f
		outputCloser = f
		fmt.Fprintf(os.Stderr, "[+] Writing to:  %s\n", *outputPath)

	case inputPath != "":
		outName := autoOutputName(inputPath)
		f, err := os.Create(outName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[!] failed to create output file: %v\n", err)
			os.Exit(1)
		}
		output = f
		outputCloser = f
		fmt.Fprintf(os.Stderr, "[+] Writing to:  %s\n", outName)
	}

	if outputCloser != nil {
		defer outputCloser.Close()
	}

	// ----- Cancellation ----------------------------------------------------
	//
	// On SIGINT/SIGTERM we stop reading new input but let the workers
	// drain whatever they have already queued. The collector still
	// flushes its results, so a Ctrl-C mid-run does not lose data that
	// was already fetched from the file/pipe.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ----- Pipeline --------------------------------------------------------
	//
	// reader → urls → workers → params → collector
	//
	// The reader is a single goroutine because input is sequential. The
	// workers form a fixed-size pool so that concurrency is bounded even
	// on a machine with thousands of cores, which keeps memory stable
	// and avoids thrashing on the shared channels.

	parser := New(nil)

	var urlsRead, urlsParsed atomic.Int64

	urls := make(chan string, channelBuffer)
	params := make(chan string, channelBuffer)

	// Reader: push trimmed, non-empty lines into the pipeline.
	go func() {
		defer close(urls)
		inputScanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
		for inputScanner.Scan() {
			line := strings.TrimSpace(inputScanner.Text())
			if line == "" {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case urls <- line:
				if n := urlsRead.Add(1); *verbose && n%progressEvery == 0 {
					fmt.Fprintf(os.Stderr, "[*] %d URLs read\n", n)
				}
			}
		}
		if err := inputScanner.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "[!] scanner error: %v\n", err)
		}
	}()

	// Workers: parse URLs and emit parameter names.
	//
	// Each worker keeps a local deduplication set. This is not required
	// for correctness — the collector deduplicates globally — but it
	// keeps the shared params channel from being flooded with the same
	// name over and over, which matters on inputs where a single common
	// parameter appears in every URL.
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			localSeen := make(map[string]struct{})
			for rawURL := range urls {
				urlsParsed.Add(1)
				for _, p := range parser.Extract(rawURL) {
					if _, dup := localSeen[p]; dup {
						continue
					}
					localSeen[p] = struct{}{}
					params <- p
				}
			}
		}()
	}

	// Closer: shut down params once every worker has finished, so that
	// the collector's range loop terminates cleanly.
	go func() {
		wg.Wait()
		close(params)
	}()

	// ----- Collector -------------------------------------------------------
	//
	// Two output modes:
	//
	//   * streaming (default): write each new parameter as it arrives.
	//     Uses O(unique) memory for the deduplication set, and produces
	//     the answer as fast as the workers can find it.
	//
	//   * sorted (-sort): buffer everything, sort, then write. Adds
	//     O(unique) memory but makes downstream diffing and diffing
	//     against previous runs deterministic.

	globalSeen := make(map[string]struct{})
	var buffered []string

	for p := range params {
		if _, dup := globalSeen[p]; dup {
			continue
		}
		globalSeen[p] = struct{}{}
		if *doSort {
			buffered = append(buffered, p)
		} else {
			fmt.Fprintln(output, p)
		}
	}

	if *doSort {
		sort.Strings(buffered)
		for _, p := range buffered {
			fmt.Fprintln(output, p)
		}
	}

	if *verbose {
		fmt.Fprintf(os.Stderr, "[+] Done. %d URLs parsed, %d unique parameters.\n",
			urlsParsed.Load(), len(globalSeen))
	}
}
