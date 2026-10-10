package waf

import (
	"strings"
	"testing"
)

func fixtureChallenge() Challenge {
	return Challenge{
		Type: "GET", SceneID: "fixture-scene", UserID: "fixture-user", UserUserID: "fixture-user-user",
		TraceID: "fixture-trace", Token: "fixture-token", Region: "cn",
	}
}

func TestChallengeValidateRequiredFieldByteBoundaries(t *testing.T) {
	for _, field := range []struct {
		name string
		set  func(*Challenge, string)
	}{
		{"sceneId", func(challenge *Challenge, value string) { challenge.SceneID = value }},
		{"userId", func(challenge *Challenge, value string) { challenge.UserID = value }},
		{"userUserId", func(challenge *Challenge, value string) { challenge.UserUserID = value }},
		{"traceid", func(challenge *Challenge, value string) { challenge.TraceID = value }},
		{"token", func(challenge *Challenge, value string) { challenge.Token = value }},
	} {
		t.Run(field.name, func(t *testing.T) {
			for _, value := range []string{"a", strings.Repeat("a", 4096), strings.Repeat("界", 1365) + "a", " fixture value \n"} {
				challenge := fixtureChallenge()
				field.set(&challenge, value)
				before := challenge
				if err := challenge.Validate(); err != nil || challenge != before {
					t.Fatalf("valid field was rejected or modified: bytes=%d error=%v", len(value), err)
				}
			}
			for _, value := range []string{"", " \t\r\n", "\u3000\u00a0", strings.Repeat("a", 4097), strings.Repeat("界", 1366)} {
				challenge := fixtureChallenge()
				field.set(&challenge, value)
				err := challenge.Validate()
				if err == nil || err.Error() != field.name+" must contain 1..4096 bytes and non-whitespace text" {
					t.Fatalf("invalid field escaped validation or exposed its value: bytes=%d error=%v", len(value), err)
				}
			}
		})
	}
}

func TestChallengeValidateTypeRegionAndLanguage(t *testing.T) {
	for _, region := range []string{"cn", "sgp"} {
		for _, language := range []string{"", "en", " en \n", strings.Repeat("a", 64), strings.Repeat("界", 21) + "a"} {
			challenge := fixtureChallenge()
			challenge.Region, challenge.Language = region, language
			before := challenge
			if err := challenge.Validate(); err != nil || challenge != before {
				t.Fatalf("valid challenge was rejected or modified: error=%v", err)
			}
		}
	}
	for _, test := range []struct {
		name, rule string
		mutate     func(*Challenge)
	}{
		{"missing type", "type must be GET", func(challenge *Challenge) { challenge.Type = "" }},
		{"unsupported type", "type must be GET", func(challenge *Challenge) { challenge.Type = "private-type" }},
		{"missing region", "region must be cn or sgp", func(challenge *Challenge) { challenge.Region = "" }},
		{"unsupported region", "region must be cn or sgp", func(challenge *Challenge) { challenge.Region = "private-region" }},
		{"region whitespace", "region must be cn or sgp", func(challenge *Challenge) { challenge.Region = " cn" }},
		{"region casing", "region must be cn or sgp", func(challenge *Challenge) { challenge.Region = "CN" }},
		{"long language", "language must contain at most 64 bytes", func(challenge *Challenge) { challenge.Language = strings.Repeat("a", 65) }},
		{"long Unicode language", "language must contain at most 64 bytes", func(challenge *Challenge) { challenge.Language = strings.Repeat("界", 22) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			challenge := fixtureChallenge()
			test.mutate(&challenge)
			if err := challenge.Validate(); err == nil || err.Error() != test.rule {
				t.Fatalf("invalid challenge escaped validation or exposed its value: error=%v", err)
			}
		})
	}
}
