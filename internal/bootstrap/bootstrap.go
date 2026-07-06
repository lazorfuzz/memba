// Package bootstrap holds the shared cold-start helpers of spec §20, used
// by both memctl bootstrap (client-side, local checkout) and
// POST /v1/admin/bootstrap (server-side, over the verifier's clone).
package bootstrap

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Globs are the §20 step-1 ingest targets: docs, ownership, build files,
// CI configs.
var Globs = []string{
	"README*", "readme*", "docs/*.md", "docs/**/*.md", "doc/*.md", "doc/**/*.md",
	"adr/*.md", "adr/**/*.md", "ADR*/*.md",
	"CODEOWNERS", ".github/CODEOWNERS", "Makefile", "justfile", "package.json",
	".github/workflows/*.yml", ".github/workflows/*.yaml", "Dockerfile", "docker-compose.yml",
}

// File is one collected bootstrap file.
type File struct {
	Rel  string
	Body string
}

// CollectFiles gathers the Globs under dir (≤ 1 MiB, UTF-8 only, deduped).
func CollectFiles(dir string) []File {
	seen := map[string]struct{}{}
	var out []File
	for _, glob := range Globs {
		matches, _ := filepath.Glob(filepath.Join(dir, glob))
		deep, _ := filepath.Glob(filepath.Join(dir, strings.ReplaceAll(glob, "**/", "")))
		for _, m := range append(matches, deep...) {
			rel, err := filepath.Rel(dir, m)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if _, dup := seen[rel]; dup {
				continue
			}
			seen[rel] = struct{}{}
			info, err := os.Stat(m)
			if err != nil || info.IsDir() || info.Size() == 0 || info.Size() > 1<<20 {
				continue
			}
			body, err := os.ReadFile(m)
			if err != nil || !utf8.Valid(body) {
				continue
			}
			out = append(out, File{Rel: rel, Body: string(body)})
		}
	}
	return out
}

var targetRe = regexp.MustCompile(`(?m)^([A-Za-z0-9][A-Za-z0-9_./-]*):(?:[^=]|$)`)

// MakeTargets extracts up to 8 Makefile/justfile targets (§10.4 extraction
// cap applies to bootstrap seeding too).
func MakeTargets(body string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, m := range targetRe.FindAllStringSubmatch(body, -1) {
		t := m[1]
		if t == "" || strings.HasPrefix(t, ".") {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
		if len(out) >= 8 {
			break
		}
	}
	return out
}
