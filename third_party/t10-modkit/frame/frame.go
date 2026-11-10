// Package frame is the frame of every t10 web page that's drawn on the server, such as
// the platform console and the account page: the rail, the Apps list and the account
// menu. React apps draw the same from the web kit's frame.tsx. Both take the Apps list
// from the auth service's homes, so a person finds the same places on every page, and
// neither names a business app.
//
// A page parses Templates into its own template set, which must have a `t` function
// that translates English, serves CSS (after its own tokens), and sets data-theme on
// <html> from Menu.Theme.
package frame

import (
	_ "embed"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Templates defines "frame-apps", "frame-account" and "frame-icon".
//
//go:embed frame.html
var Templates string

// CSS is the rail, the Apps list and the account menu, in the web kit's tokens.
//
//go:embed frame.css
var CSS []byte

// IDs of the places that aren't business apps.
const (
	Workbench = "workbench"
	Console   = "console"
	Account   = "account"
)

// Homes is the auth service's GET /v1/me/homes: everywhere this person can go.
type Homes struct {
	Workbench *struct {
		Tenants []Tenant `json:"tenants"`
	} `json:"workbench,omitempty"`
	Console *Link `json:"console,omitempty"`
	Account Link  `json:"account"`
}

// Tenant is one of this person's workspaces, with its Workbench and its apps.
type Tenant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
	URL  string `json:"url"`
	Apps []App  `json:"apps"`
}

// App is a business app this person can use in a tenant.
type App struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	LabelZH string `json:"label_zh"`
	URL     string `json:"url"`
}

// Link is an address.
type Link struct {
	URL string `json:"url"`
}

// Place is one entry in the Apps list.
type Place struct {
	ID, Label, Href string
	// Icon is "workbench", "app" or "console".
	Icon    string
	Current bool
}

// Places is the Apps list, in the order every app shows it: Workbench, the tenant's
// apps, then the platform console for a platform admin. On a page that belongs to no
// tenant, a person in one sees that tenant's places; a person in several sees each
// tenant's Workbench, named by the tenant, where their apps are one step away.
// current marks the page's own entry.
func Places(h Homes, tenant, current string, zh bool) []Place {
	places := []Place{}
	var tenants []Tenant
	if h.Workbench != nil {
		tenants = h.Workbench.Tenants
	}
	if tenant != "" {
		var here []Tenant
		for _, t := range tenants {
			if t.ID == tenant {
				here = append(here, t)
			}
		}
		tenants = here
	}
	switch {
	case len(tenants) == 1:
		t := tenants[0]
		places = append(places, Place{ID: Workbench, Label: "Workbench", Href: t.URL, Icon: "workbench", Current: current == Workbench})
		for _, a := range t.Apps {
			label := a.Label
			if zh && a.LabelZH != "" {
				label = a.LabelZH
			}
			places = append(places, Place{ID: a.ID, Label: label, Href: a.URL, Icon: "app", Current: current == a.ID})
		}
	case len(tenants) > 1:
		for _, t := range tenants {
			places = append(places, Place{ID: Workbench + ":" + t.ID, Label: t.Name, Href: t.URL, Icon: "workbench"})
		}
	}
	if h.Console != nil && h.Console.URL != "" {
		places = append(places, Place{ID: Console, Label: "Platform console", Href: h.Console.URL, Icon: "console", Current: current == Console})
	}
	return places
}

// Menu is what the account menu shows and where its forms go. Every action is a
// path on the page's own site.
type Menu struct {
	// Name is who's signed in, and Detail where: a tenant, or the platform.
	Name, Detail string
	// Lang is "en" or "zh-Hans", and Theme "system", "light" or "dark".
	Lang, Theme string
	// Here is where a change of language or theme comes back to.
	Here string
	// LanguageAction and ThemeAction take a POST with lang or theme, and next.
	LanguageAction, ThemeAction string
	// AccountURL is the account page; empty on the account page itself.
	AccountURL string
	// SignOutAction ends every t10 app in this browser.
	SignOutAction string
}

// Initial is the letter on the avatar.
func (m Menu) Initial() string {
	r, _ := utf8.DecodeRuneInString(strings.TrimSpace(m.Name))
	if r == utf8.RuneError {
		return "?"
	}
	return strings.ToUpper(string(r))
}

// ValidTheme is a theme the frame knows.
func ValidTheme(theme string) bool {
	return theme == "system" || theme == "light" || theme == "dark"
}

// AccountLink is the account page with the page to come back to, so it can offer
// "Back to" the app it was opened from. The account page checks the address.
func AccountLink(account, back string) string {
	if account == "" || back == "" {
		return account
	}
	u, err := url.Parse(account)
	if err != nil {
		return account
	}
	q := u.Query()
	q.Set("return_to", back)
	u.RawQuery = q.Encode()
	return u.String()
}
