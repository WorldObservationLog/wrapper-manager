package main

import "testing"

func TestNormalizeLangTag(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ja", "ja"}, {"ja-JP", "ja"}, {"ko", "ko"},
		{"zh-Hant-TW", "zh-hant"}, {"zh-Hans-CN", "zh-hans"}, {"zh-Hant", "zh-hant"},
		{"en-US", "en"}, {"en-GB", "en"}, {"fr-FR", "fr"},
		{"  pt-BR  ", "pt"}, {"", ""}, {"XX", "xx"},
	}
	for _, c := range cases {
		if got := normalizeLangTag(c.in); got != c.want {
			t.Errorf("normalizeLangTag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRegionSupportsLang(t *testing.T) {
	m := StorefrontLangs{
		"jp": {"ja", "en-US"},
		"kr": {"ko", "en-GB"},
		"cn": {"zh-Hans-CN", "en-GB"},
		"tw": {"zh-Hant-TW", "en-GB"},
		"th": {"en-GB", "th"},
		"us": {"en-US", "es-MX", "ko", "zh-Hant-TW"},
	}
	cases := []struct {
		region, lang string
		want         bool
	}{
		{"jp", "ja", true}, {"jp", "ja-JP", true},
		{"kr", "ko", true}, {"cn", "zh-Hans", true}, {"cn", "zh-Hans-CN", true},
		{"tw", "zh-Hant", true}, {"us", "ko", true}, {"th", "th", true},
		{"jp", "ko", false}, {"kr", "ja", false}, {"th", "ko", false},
		{"jp", "en", true}, // en-US normalized => en matches
		{"xx", "ja", false},
	}
	for _, c := range cases {
		if got := m.regionSupportsLang(c.region, c.lang); got != c.want {
			t.Errorf("regionSupportsLang(%q,%q) = %v, want %v", c.region, c.lang, got, c.want)
		}
	}
}
