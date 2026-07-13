package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestOperatorPagesExposeAccessibleResponsiveShells(t *testing.T) {
	app := newIsolatedTestApp(t)
	assertAccessiblePage(t, app.Handler, nil, "/setup")
	cookie := app.authenticate(t)
	assertAccessiblePage(t, app.Handler, nil, "/login")
	for _, path := range []string{"/", "/manage", "/observability", "/admin"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.AddCookie(cookie)
			app.Handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("GET %s = %d", path, recorder.Code)
			}
			body := recorder.Body.String()
			for _, contract := range []string{
				`<meta name="viewport"`, `<nav class="app-nav" aria-label="Main navigation">`, `<main`,
				`id="logout-btn"`, `type="button"`,
			} {
				if !strings.Contains(body, contract) {
					t.Errorf("GET %s missing accessibility contract %q", path, contract)
				}
			}
			assertAccessibleHTML(t, path, strings.NewReader(body))
		})
	}
}

func TestOperatorPagesRenderOneSharedApplicationShellContract(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	pages := []struct {
		path        string
		pageLabel   string
		currentHref string
		landmark    string
	}{
		{path: "/", pageLabel: "Status", currentHref: "/", landmark: "Fleet status"},
		{path: "/manage", pageLabel: "Manage Servers", currentHref: "/manage", landmark: "Server directory"},
		{path: "/observability", pageLabel: "Observability", currentHref: "/observability", landmark: "Update metrics"},
		{path: "/admin", pageLabel: "Admin", currentHref: "/admin", landmark: "App Time"},
	}
	wantLinks := []struct {
		label string
		href  string
	}{
		{label: "Status", href: "/"},
		{label: "Manage Servers", href: "/manage"},
		{label: "Observability", href: "/observability"},
		{label: "Admin", href: "/admin"},
	}

	for _, page := range pages {
		t.Run(page.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, page.path, nil)
			request.AddCookie(cookie)
			app.Handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("GET %s = %d", page.path, recorder.Code)
			}
			body := recorder.Body.String()
			if !strings.Contains(body, page.landmark) {
				t.Fatalf("GET %s lost page landmark %q", page.path, page.landmark)
			}
			root, err := html.Parse(strings.NewReader(body))
			if err != nil {
				t.Fatalf("parse %s: %v", page.path, err)
			}
			shells := htmlElements(root, "header", "app-header")
			if len(shells) != 1 {
				t.Fatalf("GET %s application shells = %d, want 1", page.path, len(shells))
			}
			if got, want := htmlAttr(shells[0], "aria-label"), page.pageLabel+" application shell"; got != want {
				t.Errorf("GET %s shell aria-label = %q, want %q", page.path, got, want)
			}
			navigations := htmlElements(shells[0], "nav", "app-nav")
			if len(navigations) != 1 {
				t.Fatalf("GET %s shell navigations = %d, want 1", page.path, len(navigations))
			}
			links := directElements(navigations[0], "a")
			if len(links) != len(wantLinks) {
				t.Fatalf("GET %s navigation links = %d, want %d", page.path, len(links), len(wantLinks))
			}
			currentCount := 0
			for index, link := range links {
				if got := strings.TrimSpace(nodeText(link)); got != wantLinks[index].label {
					t.Errorf("GET %s navigation link %d label = %q, want %q", page.path, index, got, wantLinks[index].label)
				}
				if got := htmlAttr(link, "href"); got != wantLinks[index].href {
					t.Errorf("GET %s navigation link %d href = %q, want %q", page.path, index, got, wantLinks[index].href)
				}
				if htmlAttr(link, "aria-current") == "page" {
					currentCount++
					if got := htmlAttr(link, "href"); got != page.currentHref {
						t.Errorf("GET %s current navigation href = %q, want %q", page.path, got, page.currentHref)
					}
				}
			}
			if currentCount != 1 {
				t.Errorf("GET %s current navigation items = %d, want 1", page.path, currentCount)
			}
		})
	}
}

func htmlElements(root *html.Node, tag, className string) []*html.Node {
	var matches []*html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == tag && hasHTMLClass(node, className) {
			matches = append(matches, node)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return matches
}

func directElements(root *html.Node, tag string) []*html.Node {
	var matches []*html.Node
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.Data == tag {
			matches = append(matches, child)
		}
	}
	return matches
}

func hasHTMLClass(node *html.Node, className string) bool {
	for _, class := range strings.Fields(htmlAttr(node, "class")) {
		if class == className {
			return true
		}
	}
	return false
}

func assertAccessiblePage(t *testing.T, handler http.Handler, cookie *http.Cookie, path string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, recorder.Code)
	}
	assertAccessibleHTML(t, path, strings.NewReader(recorder.Body.String()))
}

func assertAccessibleHTML(t *testing.T, path string, reader io.Reader) {
	t.Helper()
	root, err := html.Parse(reader)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	ids := map[string]bool{}
	labels := map[string]bool{}
	var nodes []*html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			nodes = append(nodes, node)
			if id := htmlAttr(node, "id"); id != "" {
				ids[id] = true
			}
			if node.Data == "label" {
				labels[htmlAttr(node, "for")] = true
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	for _, node := range nodes {
		switch node.Data {
		case "img":
			if _, ok := htmlAttrPresent(node, "alt"); !ok {
				t.Errorf("%s contains an image without alt text", path)
			}
		case "button":
			if strings.TrimSpace(nodeText(node)) == "" && htmlAttr(node, "aria-label") == "" {
				t.Errorf("%s contains a button without an accessible name", path)
			}
		case "input", "select", "textarea":
			if htmlAttr(node, "type") == "hidden" {
				continue
			}
			id := htmlAttr(node, "id")
			if htmlAttr(node, "aria-label") == "" && htmlAttr(node, "aria-labelledby") == "" && !hasLabelAncestor(node) && (id == "" || !labels[id]) {
				t.Errorf("%s contains an unlabeled %s#%s", path, node.Data, id)
			}
		}
		if htmlAttr(node, "role") == "dialog" {
			labelledBy := htmlAttr(node, "aria-labelledby")
			if htmlAttr(node, "aria-modal") != "true" || labelledBy == "" || !ids[labelledBy] {
				t.Errorf("%s contains a dialog without modal and title contracts", path)
			}
		}
	}
}

func hasLabelAncestor(node *html.Node) bool {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == html.ElementNode && parent.Data == "label" {
			return true
		}
	}
	return false
}

func htmlAttr(node *html.Node, name string) string {
	value, _ := htmlAttrPresent(node, name)
	return value
}

func htmlAttrPresent(node *html.Node, name string) (string, bool) {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val, true
		}
	}
	return "", false
}

func nodeText(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			builder.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return builder.String()
}

func TestOperatorStylesCoverRepresentativeViewportsAndScrollableTables(t *testing.T) {
	for _, path := range []string{"static/css/base.css", "static/css/index.css", "static/css/manage.css", "static/css/observability.css", "static/css/admin.css"} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		css := string(contents)
		if !strings.Contains(css, "@media (max-width:") {
			t.Errorf("%s has no constrained viewport behavior", path)
		}
	}
	base, err := os.ReadFile("static/css/base.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(base), "overflow-x: auto") {
		t.Error("shared table foundation does not preserve horizontal access at constrained widths")
	}
	status, err := os.ReadFile("static/css/index.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, breakpoint := range []string{"max-width: 1380px", "max-width: 980px", "max-width: 720px"} {
		if !strings.Contains(string(status), breakpoint) {
			t.Errorf("Status viewport smoke coverage is missing %s", breakpoint)
		}
	}
}
