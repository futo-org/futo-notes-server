package country_test

import (
	"testing"

	"golang.org/x/text/language"

	"futo-notes-server/internal/hosted/country"
)

func TestList(t *testing.T) {
	all := country.All()
	if len(all) != 252 {
		t.Fatalf("%d countries, want ISO 3166-1's 249 plus XK, AC and TA", len(all))
	}
	seen := map[string]bool{}
	for _, c := range all {
		region, err := language.ParseRegion(c.Code)
		if err != nil || region.String() != c.Code || c.Name == "" {
			t.Errorf("%+v is not a region code with a name (%v)", c, err)
		}
		if seen[c.Code] {
			t.Errorf("%s listed twice", c.Code)
		}
		seen[c.Code] = true
	}
	for code, name := range map[string]string{"GB": "United Kingdom", "US": "United States", "XK": "Kosovo", "DE": "Germany"} {
		if i := indexOf(all, code); i < 0 || all[i].Name != name {
			t.Errorf("%s missing or misnamed", code)
		}
	}
	if all[0].Code != "AF" || all[1].Code != "AX" || all[len(all)-1].Code != "ZW" {
		t.Errorf("not sorted by name: first %v, last %v", all[0], all[len(all)-1])
	}
}

func indexOf(all []country.Country, code string) int {
	for i, c := range all {
		if c.Code == code {
			return i
		}
	}
	return -1
}

func TestParse(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"GB", "GB"}, {"gb", "GB"}, {" us ", "US"}, {"xk", "XK"}, {"ta", "TA"}} {
		if got, ok := country.Parse(tc.in); !ok || got != tc.want {
			t.Errorf("Parse(%q) = %q, %t", tc.in, got, ok)
		}
	}
	// Not countries: unknown, private use, macro regions, deprecated,
	// numeric, and alpha-3.
	for _, in := range []string{"", "ZZ", "QO", "EU", "UK", "AN", "419", "GBR", "G", "<b>"} {
		if got, ok := country.Parse(in); ok {
			t.Errorf("Parse(%q) = %q, accepted", in, got)
		}
	}
}

func TestName(t *testing.T) {
	for code, want := range map[string]string{"US": "United States", "CA": "Canada", "XK": "Kosovo", "ZZ": "ZZ", "": ""} {
		if got := country.Name(code); got != want {
			t.Errorf("Name(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestFromAcceptLanguage(t *testing.T) {
	for header, want := range map[string]string{
		"en-GB,en;q=0.9":            "GB",
		"en-US,en;q=0.5":            "US",
		"fr-CH, fr;q=0.9, en;q=0.8": "CH",
		"en;q=0.9, de-AT":           "AT",
		"zh-Hant-TW":                "TW",
		"de":                        "",
		"en":                        "",
		"es-419,es;q=0.9":           "",
		"es-419,es-MX;q=0.8":        "MX",
		"":                          "",
		"*":                         "",
		"not a header ;;; q=x":      "",
		"en-GB;q=0.1, pt-BR;q=0.9":  "BR",
		"en-ZZ, en-GB;q=0.5":        "GB",
		"sr-Latn-XK":                "XK",
		"en-gb":                     "GB",
		"en-GB;q=0, nl-NL;q=0.000":  "",
		"en-001, en-IE;q=0.7":       "IE",
		"x-private, it-IT;q=0.6":    "IT",
		"en-US-u-rg-gbzzzz;q=1, fr": "US",
	} {
		if got := country.FromAcceptLanguage(header); got != want {
			t.Errorf("FromAcceptLanguage(%q) = %q, want %q", header, got, want)
		}
	}
}
