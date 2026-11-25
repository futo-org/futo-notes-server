// Package country is the billing countries web checkout asks for: ISO
// 3166-1 alpha-2 codes with their English names, and the country a
// browser's preferred languages suggest.
package country

import (
	"slices"
	"strings"

	"golang.org/x/text/language"
)

// Country is a billing country.
type Country struct {
	// Code is the ISO 3166-1 alpha-2 code, upper case.
	Code string
	// Name is its English name.
	Name string
}

// All lists every country, sorted by name. Callers must not modify it.
func All() []Country { return all }

// Parse returns the country code s names, in any case, and whether it is
// one of All.
func Parse(s string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(s))
	if !slices.ContainsFunc(all, func(c Country) bool { return c.Code == code }) {
		return "", false
	}
	return code, true
}

// Name returns the English name of the country code names, or code itself
// when it is not one of All.
func Name(code string) string {
	if i := slices.IndexFunc(all, func(c Country) bool { return c.Code == code }); i >= 0 {
		return all[i].Name
	}
	return code
}

// FromAcceptLanguage returns the country of the most preferred language in
// an Accept-Language header that names one explicitly ("en-GB" is GB), or
// "" when none does. A bare language ("en") names no country: the region
// it most likely means is a guess.
func FromAcceptLanguage(header string) string {
	tags, _, err := language.ParseAcceptLanguage(header)
	if err != nil {
		return ""
	}
	for _, tag := range tags {
		region, confidence := tag.Region()
		if confidence != language.Exact {
			continue
		}
		if code, ok := Parse(region.String()); ok {
			return code
		}
	}
	return ""
}
