// Package waf 获取 WAF 页面的单轮上下文，并通过内嵌验证码运行时生成结果。
package waf

import (
	"encoding/json"
	"errors"
	"html"
	"regexp"
	"strings"
	"unicode/utf8"

	domainwaf "github.com/d2120848471/v3/ali-slider-go/internal/domain/waf"
)

const MaxPageBytes = 2 << 20

var (
	ErrUnsupportedPage = errors.New("unsupported WAF challenge page")
	textareaPattern    = regexp.MustCompile(`(?is)<textarea\b(?:[^"'<>]|"[^"]*"|'[^']*')*>(.*?)</textarea\s*>`)
	assignmentPattern  = regexp.MustCompile(`^\s*var\s+requestInfo\s*=\s*`)
)

// ParseChallenge 仅接受 textarea 内的 JSON 赋值，不执行页面脚本或 JS 表达式。
func ParseChallenge(page []byte) (domainwaf.Challenge, error) {
	if len(page) == 0 || len(page) > MaxPageBytes || !utf8.Valid(page) {
		return domainwaf.Challenge{}, ErrUnsupportedPage
	}
	var challenge domainwaf.Challenge
	found := false
	for _, match := range textareaPattern.FindAllSubmatch(page, -1) {
		content := html.UnescapeString(string(match[1]))
		assignment := assignmentPattern.FindStringIndex(content)
		if assignment == nil {
			continue
		}
		// 多个上下文不能可靠归属于同一轮挑战，直接拒绝。
		if found {
			return domainwaf.Challenge{}, ErrUnsupportedPage
		}
		found = true
		content = content[assignment[1]:]
		decoder := json.NewDecoder(strings.NewReader(content))
		if err := decoder.Decode(&challenge); err != nil {
			return domainwaf.Challenge{}, ErrUnsupportedPage
		}
		tail := strings.TrimSpace(content[decoder.InputOffset():])
		if tail != "" && tail != ";" {
			return domainwaf.Challenge{}, ErrUnsupportedPage
		}
	}
	if !found || challenge.Validate() != nil {
		return domainwaf.Challenge{}, ErrUnsupportedPage
	}
	return challenge, nil
}
