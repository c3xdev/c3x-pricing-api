package db

import "testing"

func FuzzMatchRegexPattern(f *testing.F) {
	f.Add("/test/i", "Test Value")
	f.Add("/^aws/", "aws_instance")
	f.Add("(?!excluded)", "included")
	f.Add("/foo|bar/", "baz")
	f.Add("plain", "plain text")
	f.Fuzz(func(t *testing.T, pattern, value string) {
		// Should never panic
		_ = MatchRegexPattern(pattern, value)
	})
}
