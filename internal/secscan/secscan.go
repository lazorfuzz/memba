// Package secscan implements ingest-time security scanning (spec §15.3,
// §15.4 D2): secret detection (gitleaks-style rules + Shannon entropy) with
// span-level redaction, and prompt-injection pattern detection.
package secscan

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

// SecretFinding is one detected secret span.
type SecretFinding struct {
	Rule  string
	Match string
	Start int
	End   int
}

type secretRule struct {
	name string
	re   *regexp.Regexp
}

// A pragmatic subset of the gitleaks default ruleset (spec §4.4
// `ruleset: gitleaks-default`); extend by appending rules.
var secretRules = []secretRule{
	{"aws_access_key", regexp.MustCompile(`\b(A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`)},
	{"aws_secret_key", regexp.MustCompile(`(?i)aws[_\-\.]?(secret)?[_\-\.]?(access)?[_\-\.]?key(?:_id)?['"\s:=]+[A-Za-z0-9/+=]{40}`)},
	{"github_token", regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b`)},
	{"github_pat", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{80,}\b`)},
	{"gitlab_token", regexp.MustCompile(`\bglpat-[A-Za-z0-9\-_]{20,}\b`)},
	{"slack_token", regexp.MustCompile(`\bxox[bpoas]-[A-Za-z0-9\-]{10,}\b`)},
	{"stripe_key", regexp.MustCompile(`\b(sk|rk)_(test|live)_[A-Za-z0-9]{20,}\b`)},
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9\-_]{20,}\b`)},
	{"openai_key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{40,}\b`)},
	{"private_key_block", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY(?: BLOCK)?-----`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`)},
	{"generic_api_key", regexp.MustCompile(`(?i)(?:api[_\-]?key|apikey|secret|token|passwd|password)['"]?\s*[:=]\s*['"][A-Za-z0-9/+=_\-]{16,}['"]`)},
	{"basic_auth_url", regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^/\s:@]{3,}:[^/\s:@]{3,}@`)},
}

// candidate high-entropy tokens: base64/hex-like runs ≥ 20 chars (spec §15.3).
var entropyCandidate = regexp.MustCompile(`\b[A-Za-z0-9/+=_\-]{20,}\b`)

// ShannonEntropy returns bits/char of s.
func ShannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	freq := map[rune]float64{}
	for _, r := range s {
		freq[r]++
	}
	n := float64(len([]rune(s)))
	var h float64
	for _, c := range freq {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

// ScanSecrets finds secret spans in body. entropyThreshold is the Shannon
// cutoff for the generic high-entropy rule (default 4.5, spec §4.4).
func ScanSecrets(body string, entropyThreshold float64) []SecretFinding {
	var out []SecretFinding
	claimed := make([]bool, len(body))
	claim := func(start, end int) {
		for i := start; i < end && i < len(claimed); i++ {
			claimed[i] = true
		}
	}
	for _, rule := range secretRules {
		for _, loc := range rule.re.FindAllStringIndex(body, -1) {
			out = append(out, SecretFinding{Rule: rule.name, Match: body[loc[0]:loc[1]], Start: loc[0], End: loc[1]})
			claim(loc[0], loc[1])
		}
	}
	for _, loc := range entropyCandidate.FindAllStringIndex(body, -1) {
		if claimed[loc[0]] {
			continue
		}
		tok := body[loc[0]:loc[1]]
		// Skip obvious non-secrets: long words, paths, UUID-ish with low charset mix.
		if ShannonEntropy(tok) >= entropyThreshold && hasMixedCharset(tok) {
			out = append(out, SecretFinding{Rule: "high_entropy", Match: tok, Start: loc[0], End: loc[1]})
			claim(loc[0], loc[1])
		}
	}
	return out
}

func hasMixedCharset(s string) bool {
	var upper, lower, digit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	return (upper && lower && digit) || (digit && (upper || lower) && len(s) >= 32)
}

// Redact replaces each finding span with ⟦REDACTED:<rule>⟧ (spec §15.3).
// Returns the redacted body and the markers used.
func Redact(body string, findings []SecretFinding) (string, []string) {
	if len(findings) == 0 {
		return body, nil
	}
	// Replace from the end so offsets stay valid.
	ordered := append([]SecretFinding(nil), findings...)
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j].Start > ordered[i].Start {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	markers := map[string]struct{}{}
	b := body
	prevStart := len(b) + 1
	for _, f := range ordered {
		if f.End > prevStart { // overlapping span already redacted
			continue
		}
		marker := fmt.Sprintf("⟦REDACTED:%s⟧", f.Rule)
		b = b[:f.Start] + marker + b[f.End:]
		markers[marker] = struct{}{}
		prevStart = f.Start
	}
	var list []string
	for m := range markers {
		list = append(list, m)
	}
	return b, list
}

// ---------------------------------------------------------------------------
// Injection scanning (spec §15.4 D2)
// ---------------------------------------------------------------------------

// InjectionFinding is one suspected prompt-injection signal.
type InjectionFinding struct {
	Pattern string
	Match   string
}

var injectionPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"ignore_previous", regexp.MustCompile(`(?i)\b(ignore|disregard|forget)\s+(all\s+)?(your\s+|the\s+)?(previous|prior|above|earlier)\s+(instructions?|prompts?|rules?|context)`)},
	{"new_instructions", regexp.MustCompile(`(?i)\b(your\s+new\s+instructions?\s+(are|is)|you\s+must\s+now\s+(follow|obey)|from\s+now\s+on\s+you\s+(are|must|will))`)},
	{"system_prompt_probe", regexp.MustCompile(`(?i)\b(reveal|print|show|repeat)\b.{0,40}\b(system\s+prompt|hidden\s+instructions?)`)},
	{"imperative_to_agent", regexp.MustCompile(`(?i)\b(hey\s+)?(ai|assistant|agent|claude|copilot|model)\s*[,:]?\s+(you\s+)?(must|should|need\s+to|have\s+to)\s+(?:(?:now|please|first|immediately)\s+)?(run|execute|delete|send|exfiltrate|post|curl|fetch|ignore|forward|add)`)},
	{"tool_call_in_prose", regexp.MustCompile(`(?i)["']?(tool_use|tool_calls?|function_call)["']?\s*[:=]\s*[{\["]`)},
	{"exfil_instruction", regexp.MustCompile(`(?i)\b(send|post|upload|exfiltrate|forward)\b.{0,60}\b(credentials?|secrets?|tokens?|api\s+keys?|environment\s+variables?)\b.{0,60}\b(to|at)\s+https?://`)},
	{"base64_blob_in_chat", regexp.MustCompile(`\b[A-Za-z0-9+/]{200,}={0,2}\b`)},
}

// ScanInjection reports suspected prompt-injection content. kind is the chunk
// kind ("chat"/"prose"/...): the base64-blob rule only applies to chat/prose,
// where an opaque blob has no legitimate reason to exist.
func ScanInjection(body, kind string) []InjectionFinding {
	var out []InjectionFinding
	for _, p := range injectionPatterns {
		if p.name == "base64_blob_in_chat" && kind != "chat" && kind != "prose" {
			continue
		}
		if m := p.re.FindString(body); m != "" {
			out = append(out, InjectionFinding{Pattern: p.name, Match: truncate(m, 120)})
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Summary renders findings for a quarantine_reason column.
func Summary(secrets []SecretFinding, injections []InjectionFinding) string {
	var parts []string
	if len(secrets) > 0 {
		rules := map[string]struct{}{}
		for _, f := range secrets {
			rules[f.Rule] = struct{}{}
		}
		var names []string
		for r := range rules {
			names = append(names, r)
		}
		parts = append(parts, "secret:"+strings.Join(names, ","))
	}
	if len(injections) > 0 {
		var names []string
		for _, f := range injections {
			names = append(names, f.Pattern)
		}
		parts = append(parts, "injection_suspect:"+strings.Join(names, ","))
	}
	return strings.Join(parts, ";")
}
