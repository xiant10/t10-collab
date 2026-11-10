package frame

import (
	"html/template"
	"strings"
	"testing"
)

func homes() Homes {
	h := Homes{Console: &Link{URL: "http://console"}, Account: Link{URL: "http://auth/v1/account"}}
	h.Workbench = &struct {
		Tenants []Tenant `json:"tenants"`
	}{Tenants: []Tenant{
		{ID: "a", Name: "Acme", URL: "http://wb/t/a/", Apps: []App{{ID: "deals", Label: "Deals", LabelZH: "交易", URL: "http://deals/t/a/"}}},
		{ID: "b", Name: "Beta", URL: "http://wb/t/b/"},
	}}
	return h
}

func ids(places []Place) string {
	out := []string{}
	for _, p := range places {
		out = append(out, p.ID+"="+p.Label)
	}
	return strings.Join(out, ",")
}

func TestPlacesInEveryAppsOrder(t *testing.T) {
	h := homes()
	if got := ids(Places(h, "a", Console, true)); got != "workbench=Workbench,deals=交易,console=Platform console" {
		t.Errorf("one tenant: %s", got)
	}
	if got := ids(Places(h, "", Account, false)); got != "workbench:a=Acme,workbench:b=Beta,console=Platform console" {
		t.Errorf("no tenant, several: %s", got)
	}
	h.Console = nil
	h.Workbench.Tenants = h.Workbench.Tenants[:1]
	if got := ids(Places(h, "", Account, false)); got != "workbench=Workbench,deals=Deals" {
		t.Errorf("no tenant, one, no console: %s", got)
	}
	if got := Places(Homes{}, "", Account, false); len(got) != 0 {
		t.Errorf("nowhere: %v", got)
	}
}

func TestTemplatesRender(t *testing.T) {
	set := template.Must(template.New("").Funcs(template.FuncMap{"t": func(s string) string { return s }}).Parse(Templates))
	var out strings.Builder
	if err := set.ExecuteTemplate(&out, "frame-apps", Places(homes(), "a", "deals", false)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `href="http://deals/t/a/" aria-current="page"`) || !strings.Contains(out.String(), "Platform console") {
		t.Errorf("apps: %s", out.String())
	}
	out.Reset()
	menu := Menu{Name: "wei", Detail: "Acme", Lang: "zh-Hans", Theme: "dark", Here: "/x", LanguageAction: "/language", ThemeAction: "/theme", AccountURL: "http://auth/v1/account", SignOutAction: "/auth/logout"}
	if err := set.ExecuteTemplate(&out, "frame-account", menu); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`value="zh-Hans" lang="zh-Hans" aria-pressed="true"`, `value="dark" aria-pressed="true"`, `href="http://auth/v1/account"`, `action="/auth/logout"`, ">W<"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("menu lacks %s: %s", want, out.String())
		}
	}
}

func TestAccountLinkCarriesThePageToComeBackTo(t *testing.T) {
	if got := AccountLink("http://auth/v1/account", "http://console/tenants"); got != "http://auth/v1/account?return_to=http%3A%2F%2Fconsole%2Ftenants" {
		t.Error(got)
	}
	if got := AccountLink("http://auth/v1/account", ""); got != "http://auth/v1/account" {
		t.Error(got)
	}
}
