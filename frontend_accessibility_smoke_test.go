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
