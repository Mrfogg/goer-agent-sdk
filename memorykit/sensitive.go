package memorykit

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// sensitivePatterns match credentials by shape. The list is deliberately short
// and high-signal: a memory store holds user preferences, so a false positive
// costs more than a missed exotic secret format.
var sensitivePatterns = []struct {
	kind    string
	pattern *regexp.Regexp
}{
	{"a private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"an API key", regexp.MustCompile(`\b(?:sk|rk)-[A-Za-z0-9_-]{16,}`)},
	{"a GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"an AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{12,}`)},
	{"a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}`)},
	{"a session token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`)},
	{"a credential", regexp.MustCompile(`(?i)(?:password|passwd|pwd|secret|api[ _-]?key|access[ _-]?token|密码|口令|私钥)\s*(?::|：|=|是|is)\s*\S{6,}`)},
}

var (
	// digitRunPattern catches card numbers written with spaces or dashes.
	digitRunPattern = regexp.MustCompile(`\d[\d -]{10,}\d`)
	// nationalIDPattern catches 18-character national ID numbers.
	nationalIDPattern = regexp.MustCompile(`\b\d{17}[\dXx]\b`)
)

var nationalIDWeights = []int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
var nationalIDChecks = []byte{'1', '0', 'X', '9', '8', '7', '6', '5', '4', '3', '2'}

// detectSensitive reports the kind of secret content looks like. The check runs
// before anything is written, so a secret never reaches the table in the first
// place.
func detectSensitive(content string) (string, bool) {
	for _, entry := range sensitivePatterns {
		if entry.pattern.MatchString(content) {
			return entry.kind, true
		}
	}

	for _, match := range digitRunPattern.FindAllString(content, -1) {
		digits := digitsOnly(match)
		if len(digits) >= 13 && len(digits) <= 19 && luhnValid(digits) {
			return "a payment card number", true
		}
	}

	for _, match := range nationalIDPattern.FindAllString(content, -1) {
		if nationalIDValid(match) {
			return "a national ID number", true
		}
	}

	return "", false
}

// sensitiveError tells the model what to do next, not just what went wrong. The
// point is to stop it from retrying with the value split up or masked.
func sensitiveError(kind string) string {
	return fmt.Sprintf(
		"content looks like %s, which is never stored in memory. Do not retry with the value split apart, masked, or rephrased: drop the value and record only the user's durable preference if there is one.",
		kind,
	)
}

func digitsOnly(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func luhnValid(digits string) bool {
	parity := len(digits) % 2
	sum := 0
	for i := 0; i < len(digits); i++ {
		digit := int(digits[i] - '0')
		if i%2 == parity {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum += digit
	}
	return sum%10 == 0
}

func nationalIDValid(id string) bool {
	if len(id) != 18 {
		return false
	}

	sum := 0
	for i := 0; i < 17; i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
		sum += int(id[i]-'0') * nationalIDWeights[i]
	}

	check := byte(unicode.ToUpper(rune(id[17])))
	return nationalIDChecks[sum%11] == check
}
