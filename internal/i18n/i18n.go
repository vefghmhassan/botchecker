// Package i18n renders the dashboard and the bot in English or Persian.
package i18n

import (
	"fmt"
	"strings"
)

// Lang is a supported interface language.
type Lang string

const (
	EN Lang = "en"
	FA Lang = "fa"
)

// Parse resolves a stored or submitted value, defaulting to English.
func Parse(s string) Lang {
	if strings.EqualFold(strings.TrimSpace(s), string(FA)) {
		return FA
	}
	return EN
}

// Dir is the text direction, for the page's dir attribute.
func (l Lang) Dir() string {
	if l == FA {
		return "rtl"
	}
	return "ltr"
}

// Code is the value for the lang attribute.
func (l Lang) Code() string { return string(l) }

// Name is how the language calls itself, for the switcher.
func (l Lang) Name() string {
	if l == FA {
		return "فارسی"
	}
	return "English"
}

// Other is the language a toggle would switch to.
func (l Lang) Other() Lang {
	if l == FA {
		return EN
	}
	return FA
}

// Langs is the selectable set, in menu order.
var Langs = []Lang{EN, FA}

// T looks up a key. Missing translations fall back to English, and then to the
// key itself — a missing string should be visible, not blank.
func T(l Lang, key string, args ...any) string {
	table := english
	if l == FA {
		table = persian
	}

	s, ok := table[key]
	if !ok {
		if s, ok = english[key]; !ok {
			return key
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

// Has reports whether a key exists at all, used by the coverage test.
func Has(key string) bool {
	_, ok := english[key]
	return ok
}

// Keys lists every known key, used by the coverage test.
func Keys() []string {
	out := make([]string, 0, len(english))
	for k := range english {
		out = append(out, k)
	}
	return out
}

// PersianKeys lists the translated keys, used by the coverage test.
func PersianKeys() []string {
	out := make([]string, 0, len(persian))
	for k := range persian {
		out = append(out, k)
	}
	return out
}

// Digits converts ASCII digits to Persian ones, for dates and counts that sit
// inside Persian sentences.
func (l Lang) Digits(s string) string {
	if l != FA {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(rune(0x06F0 + (r - '0')))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
