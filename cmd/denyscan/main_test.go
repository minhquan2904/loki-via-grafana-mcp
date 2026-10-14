package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minhquan2904/loki-via-grafana-mcp/internal/redact"
)

// newRepo makes a throwaway git repository with one committed file.
func newRepo(t *testing.T, content, message string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
	} {
		if _, err := git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-q", "-m", message}} {
		if _, err := git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestScan_FindsShapesGrepMissed(t *testing.T) {
	// \d and \S are exactly what the former POSIX-grep scanner could not match.
	dir := newRepo(t, "ip 10."+"9.8.7\nAuthorization: Bear"+"er abcdefghij\n", "init")
	var out bytes.Buffer
	hits, err := scan(dir, redact.Default(), nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	// Each line hits once in the working tree and once in history.
	if hits != 4 {
		t.Fatalf("hits = %d, want 4:\n%s", hits, out.String())
	}
	if strings.Contains(out.String(), "9.8.7") || strings.Contains(out.String(), "abcdefghij") {
		t.Fatalf("the report printed a matching value:\n%s", out.String())
	}
}

func TestScan_CommitMetadata(t *testing.T) {
	dir := newRepo(t, "clean\n", "deploy to 192."+"168.1.1")
	var out bytes.Buffer
	hits, err := scan(dir, redact.Default(), nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 || !strings.Contains(out.String(), "(author/committer/message)") {
		t.Fatalf("hits = %d, want the message hit:\n%s", hits, out.String())
	}
}

func TestScan_AckSilencesHistoryOnly(t *testing.T) {
	dir := newRepo(t, "ip 10."+"9.8.7\n", "init")
	head, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	acks := []ack{{commit: strings.TrimSpace(string(head))[:12], path: "a.txt"}}
	var out bytes.Buffer
	hits, err := scan(dir, redact.Default(), acks, &out)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 || !strings.HasPrefix(out.String(), "a.txt:1:") {
		t.Fatalf("hits = %d, want only the working-tree hit:\n%s", hits, out.String())
	}
}

func TestScan_GitFailureIsAnError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	_, err := scan(t.TempDir(), redact.Default(), nil, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("scanning a directory that is not a repository must fail, not report clean")
	}
}

func TestRun_SelfTest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "local"), []byte("# comment\nacme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", dir, "-local", filepath.Join(dir, "local"), "-self-test"}, &stdout, &stderr)
	if code != exitClean {
		t.Fatalf("self-test exit = %d\nstdout %s\nstderr %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "4 canaries detected") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestRun_RequireLocal(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"-dir", t.TempDir(), "-require-local"}, &bytes.Buffer{}, &stderr); code != exitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestLoadAcks_Malformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allow")
	if err := os.WriteFile(path, []byte("abc a.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAcks(path); err == nil {
		t.Fatalf("a short commit prefix must be refused")
	}
}
