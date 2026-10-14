// Command denyscan refuses a repository that carries internal identifiers in
// its working tree or anywhere in its history.
//
// The repository is public, so every file and every commit is a publication.
// Matching is done here, in Go, with the same RE2 patterns internal/redact uses
// for the benchmark report, so the scanner and the report can never disagree on
// what a pattern means (POSIX grep, for one, does not understand \d or \S).
//
// What is scanned:
//
//   - the working tree: tracked files plus untracked files that are not ignored,
//     so a file about to be added is caught too;
//   - the history: every line added by any commit on any ref, and each commit's
//     author, committer and message.
//
// Output names the place and the pattern, never the matching text.
//
// Usage:
//
//	denyscan [-local FILE] [-allow FILE] [-require-local] [-self-test] [-dir DIR]
//
// History cannot be edited once published, so a reviewed, harmless hit in an
// old commit (a synthetic test value, say) is acknowledged in the tracked
// .denyscan-allow file: one "<commit prefix> <path>" per line. It silences that
// file in that commit only; the working tree is never exempt.
//
// Exit 0 clean, 1 something matched, 2 a precondition failed.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/minhquan2904/loki-via-grafana-mcp/internal/redact"
)

const (
	exitClean   = 0
	exitMatched = 1
	exitUsage   = 2
)

// allowlist holds the files that legitimately spell out deny patterns.
var allowlist = map[string]bool{
	"internal/redact/redact.go": true,
	"scripts/denylist-scan.sh":  true,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("denyscan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "repository to scan")
	local := fs.String("local", ".denylist.local", "extra patterns, one RE2 regexp per line (relative to -dir)")
	requireLocal := fs.Bool("require-local", false, "fail when the local pattern file is missing")
	selfTest := fs.Bool("self-test", false, "prove the scanner catches seeded canaries, then exit")
	allowFile := fs.String("allow", ".denyscan-allow", "acknowledged history hits, \"<commit prefix> <path>\" per line (relative to -dir)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	deny := redact.Default()
	localPath := *local
	if !filepath.IsAbs(localPath) {
		localPath = filepath.Join(*dir, localPath)
	}
	hasLocal := false
	if _, err := os.Stat(localPath); err == nil {
		if err := deny.WithFile(localPath); err != nil {
			fmt.Fprintln(stderr, "denyscan:", err)
			return exitUsage
		}
		hasLocal = true
	} else if *requireLocal {
		fmt.Fprintf(stderr, "denyscan: %s is required for this run\n", *local)
		return exitUsage
	} else {
		fmt.Fprintf(stderr, "denyscan: no %s; scanning with the built-in patterns only\n", *local)
	}

	if *selfTest {
		return selfTestRun(deny, hasLocal, localPath, stdout, stderr)
	}

	allowPath := *allowFile
	if !filepath.IsAbs(allowPath) {
		allowPath = filepath.Join(*dir, allowPath)
	}
	acks, err := loadAcks(allowPath)
	if err != nil {
		fmt.Fprintln(stderr, "denyscan:", err)
		return exitUsage
	}

	hits, err := scan(*dir, deny, acks, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "denyscan:", err)
		return exitUsage
	}
	if hits > 0 {
		fmt.Fprintf(stderr, "denyscan: %d deny pattern match(es); refusing\n", hits)
		return exitMatched
	}
	fmt.Fprintln(stdout, "denyscan: clean")
	return exitClean
}

// scan checks the working tree and the history of the repository at dir and
// reports each hit on out. Any git failure is an error, never a clean result.
func scan(dir string, deny *redact.Denylist, acks []ack, out io.Writer) (int, error) {
	hits := 0
	report := func(where, pattern string) {
		fmt.Fprintf(out, "%s: pattern %s\n", where, pattern)
		hits++
	}

	files, err := git(dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return 0, err
	}
	for _, path := range strings.Split(strings.TrimRight(string(files), "\x00"), "\x00") {
		if path == "" || allowlist[path] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, path))
		if errors.Is(err, os.ErrNotExist) {
			continue // deleted in the working tree, still in the index
		}
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", path, err)
		}
		if isBinary(raw) {
			continue
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if pattern, ok := deny.Match(line); ok {
				report(fmt.Sprintf("%s:%d", path, i+1), pattern)
			}
		}
	}

	if _, err := git(dir, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return hits, nil // no commit yet: nothing in history
	}
	// %x00 separates the metadata block from the patch; the marker line lets the
	// parser tell commits apart without guessing from the patch content.
	logOut, err := git(dir, "log", "-p", "--all", "--no-color", "--no-ext-diff",
		"--format=@@denyscan-commit %H%n%an <%ae>%n%cn <%ce>%n%B%x00")
	if err != nil {
		return 0, err
	}
	scanHistory(logOut, deny, acks, report)
	return hits, nil
}

// scanHistory walks `git log -p` output: commit metadata, then the added lines
// of each file, skipping files on the allowlist.
func scanHistory(logOut []byte, deny *redact.Denylist, acks []ack, report func(where, pattern string)) {
	sc := bufio.NewScanner(bytes.NewReader(logOut))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)

	commit, path := "", ""
	inMeta := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "@@denyscan-commit "):
			commit = strings.TrimPrefix(line, "@@denyscan-commit ")
			if len(commit) > 12 {
				commit = commit[:12]
			}
			path, inMeta = "", true
			continue
		case inMeta:
			if strings.HasSuffix(line, "\x00") {
				inMeta = false
				line = strings.TrimSuffix(line, "\x00")
			}
			if pattern, ok := deny.Match(line); ok {
				report(fmt.Sprintf("commit %s (author/committer/message)", commit), pattern)
			}
			continue
		case strings.HasPrefix(line, "+++ "):
			path = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			continue
		case strings.HasPrefix(line, "+") && !allowlist[path] && !acknowledged(acks, commit, path):
			if pattern, ok := deny.Match(line[1:]); ok {
				report(fmt.Sprintf("commit %s %s", commit, path), pattern)
			}
		}
	}
}

// selfTestRun builds a throwaway repository seeded with canaries in a file, in
// history only, and in a commit message, then requires scan to catch each.
func selfTestRun(deny *redact.Denylist, hasLocal bool, localPath string, stdout, stderr io.Writer) int {
	tmp, err := os.MkdirTemp("", "denyscan-selftest-")
	if err != nil {
		fmt.Fprintln(stderr, "self-test:", err)
		return exitUsage
	}
	defer os.RemoveAll(tmp)

	// Assembled at run time so this source file stays clean of the shapes.
	canaries := map[string]string{
		"private IPv4":  "10." + "1.2.3",
		"bearer header": "Authorization: Bearer " + "abcdefghij",
		"grafana token": "glsa" + "_canary0123456789",
	}
	if hasLocal {
		sample, ok := localSample(localPath)
		if !ok {
			fmt.Fprintln(stderr, "self-test: cannot build a canary for the first local pattern; put a plain word first in the file")
			return exitUsage
		}
		canaries["local pattern"] = sample
	}

	steps := [][]string{
		{"init", "-q"},
		{"config", "user.email", "selftest@example.invalid"},
		{"config", "user.name", "selftest"},
	}
	for _, step := range steps {
		if _, err := git(tmp, step...); err != nil {
			fmt.Fprintln(stderr, "self-test:", err)
			return exitUsage
		}
	}
	for name, value := range canaries {
		file := filepath.Join(tmp, "seed.txt")
		// Each canary is committed, then removed, so it only lives in history;
		// the commit message carries it as well.
		if err := os.WriteFile(file, []byte("x "+value+"\n"), 0o600); err != nil {
			fmt.Fprintln(stderr, "self-test:", err)
			return exitUsage
		}
		for _, step := range [][]string{{"add", "seed.txt"}, {"commit", "-q", "-m", "seed " + name + ": " + value}, {"rm", "-q", "seed.txt"}, {"commit", "-q", "-m", "drop"}} {
			if _, err := git(tmp, step...); err != nil {
				fmt.Fprintln(stderr, "self-test:", err)
				return exitUsage
			}
		}
	}
	// One canary also stays in the working tree, untracked.
	if err := os.WriteFile(filepath.Join(tmp, "untracked.txt"), []byte(canaries["private IPv4"]+"\n"), 0o600); err != nil {
		fmt.Fprintln(stderr, "self-test:", err)
		return exitUsage
	}

	var out bytes.Buffer
	if _, err := scan(tmp, deny, nil, &out); err != nil {
		fmt.Fprintln(stderr, "self-test:", err)
		return exitUsage
	}
	report := out.String()
	failed := false
	if !strings.Contains(report, "untracked.txt:1:") {
		fmt.Fprintln(stderr, "self-test: an untracked file was NOT scanned")
		failed = true
	}
	for name, value := range canaries {
		pattern, ok := deny.Match(value)
		if !ok || !strings.Contains(report, "seed.txt: pattern "+pattern) {
			fmt.Fprintf(stderr, "self-test: the %s canary was NOT detected in history\n", name)
			failed = true
		}
	}
	if !strings.Contains(report, "(author/committer/message)") {
		fmt.Fprintln(stderr, "self-test: commit messages were NOT scanned")
		failed = true
	}
	if failed {
		return exitMatched
	}
	fmt.Fprintf(stdout, "self-test: %d canaries detected in history, messages and the working tree\n", len(canaries))
	return exitClean
}

// localSample turns the first local pattern into a string it matches, when the
// pattern is a plain word (letters, digits, '-', '_', '.' escaped as \.).
func localSample(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sample := strings.ReplaceAll(line, `\.`, ".")
		sample = strings.TrimPrefix(strings.TrimSuffix(sample, `\b`), `\b`)
		for _, r := range sample {
			if !(r == '-' || r == '_' || r == '.' || r == '@' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
				return "", false
			}
		}
		return sample, true
	}
	return "", false
}

// ack is one acknowledged history hit: a file in a commit.
type ack struct{ commit, path string }

// loadAcks reads the allow file; a missing file means no acknowledgements.
func loadAcks(path string) ([]ack, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("allow file %s is unreadable: %w", path, err)
	}
	var acks []ack
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) < 7 {
			return nil, fmt.Errorf("allow file %s line %d: want \"<commit prefix, 7+ chars> <path>\"", path, i+1)
		}
		acks = append(acks, ack{commit: fields[0], path: fields[1]})
	}
	return acks, nil
}

func acknowledged(acks []ack, commit, path string) bool {
	for _, a := range acks {
		if a.path == path && (strings.HasPrefix(commit, a.commit) || strings.HasPrefix(a.commit, commit)) {
			return true
		}
	}
	return false
}

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// isBinary uses git's own heuristic: a NUL byte in the first 8000 bytes.
func isBinary(raw []byte) bool {
	return bytes.IndexByte(raw[:min(len(raw), 8000)], 0) >= 0
}
