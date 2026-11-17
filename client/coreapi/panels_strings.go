package coreapi

// The texts of the panels: separate from the descriptions (which carry keys only). Each provider owns one table per
// locale, {"key": "text"}. "C" is the base and holds every key the provider's descriptions use; other locales (en_US,
// sl_SI …) lie on top of it. GET /panels/strings?lang=<locale> answers one merged table.

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxStringEntries = 2000
	baseLocale       = "C"
)

var (
	reLocale  = regexp.MustCompile(`^(C|[a-z]{2}|[a-z]{2}_[A-Z]{2})$`)
	reTextVar = regexp.MustCompile(`\{[a-z][A-Za-z0-9_]{0,63}\}`)
)

// ValidateStringTable: keys of the provider's namespace, plain texts of at most maxMessage characters without control or
// format characters, and no placeholder but {field} (a row's own field, filled in as plain text).
func ValidateStringTable(provider string, t map[string]string) error {
	if len(t) > maxStringEntries {
		return fmt.Errorf("more than %d entries", maxStringEntries)
	}
	for k, v := range t {
		if !reKey.MatchString(k) || !strings.HasPrefix(k, provider+".") {
			return fmt.Errorf("key %q is not a key of provider %s", k, provider)
		}
		if strings.TrimSpace(v) == "" || utf8.RuneCountInString(v) > maxMessage || !utf8.ValidString(v) {
			return fmt.Errorf("%s: the text is empty or longer than %d characters", k, maxMessage)
		}
		for _, r := range v {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
				return fmt.Errorf("%s: the text has a control character", k)
			}
		}
		if strings.ContainsAny(reTextVar.ReplaceAllString(v, ""), "{}") {
			return fmt.Errorf("%s: a brace that is not a {field} placeholder", k)
		}
	}
	return nil
}

// chooseLocale: the exact locale if some provider has it, else the first (sorted) locale of the same language, else C.
func chooseLocale(ps []PanelProvider, lang string) string {
	all := map[string]bool{}
	for _, p := range ps {
		for l := range p.Strings {
			all[l] = true
		}
	}
	if all[lang] {
		return lang
	}
	var same []string
	for l := range all {
		if lang != baseLocale && strings.HasPrefix(l, lang[:2]+"_") {
			same = append(same, l)
		}
	}
	sort.Strings(same)
	if len(same) > 0 {
		return same[0]
	}
	return baseLocale
}

// mergedStrings: C, then the other tables of the chosen language, then the chosen locale, per provider.
func (a *API) mergedStrings(ps []PanelProvider, locale string) map[string]string {
	out := map[string]string{}
	for _, p := range ps {
		layers := []string{baseLocale}
		var same []string
		for l := range p.Strings {
			if l != baseLocale && l != locale && locale != baseLocale && strings.HasPrefix(l, locale[:2]+"_") {
				same = append(same, l)
			}
		}
		sort.Strings(same)
		layers = append(layers, same...)
		if locale != baseLocale {
			layers = append(layers, locale)
		}
		for _, l := range layers {
			t, ok := p.Strings[l]
			if !ok {
				continue
			}
			if err := ValidateStringTable(p.ID, t); err != nil {
				a.o.Log("panels: the %s strings of provider %s are left out: %v", l, p.ID, err)
				continue
			}
			for k, v := range t {
				out[k] = v
			}
		}
	}
	return out
}

func (a *API) adminPanelStrings(rawQuery string) Response {
	q, _ := url.ParseQuery(rawQuery)
	lang := q.Get("lang")
	if lang == "" {
		lang = baseLocale
	}
	if !reLocale.MatchString(lang) {
		return resp(400, errorBody(400, "lang: C or a locale such as sl_SI"))
	}
	ps := panelProviders()
	locale := chooseLocale(ps, lang)
	return resp(200, map[string]any{"format": PanelFormat, "locale": locale, "strings": a.mergedStrings(ps, locale)})
}
