// Package tokens provides a fast, model-agnostic token estimator used for
// budget accounting (spec §8 budgets, §12.3). Estimation errs slightly high
// so budgets are respected under any real tokenizer.
package tokens

import "unicode/utf8"

// Estimate approximates the token count of s: ~1 token per 3.5 characters
// for prose/code, floor of 1 for non-empty strings.
func Estimate(s string) int {
	if s == "" {
		return 0
	}
	n := utf8.RuneCountInString(s)
	t := (n*2 + 6) / 7 // n / 3.5, rounded
	if t < 1 {
		t = 1
	}
	return t
}
