package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file locks down the **capability boundary** of the generic Webhook template.
//
// This is the only place in the whole package where a 'user-provided string is evaluated as code', so it must be made explicit what it can do,
// what it cannot do, and these properties must be pinned down with tests—otherwise someone later casually adds a
// method to the template context, or adds a readFile to the FuncMap, and the capability surface silently expands, while the diff looks
// like just a harmless little function.

// TestTemplateContextHasNoMethods is the most important one.
//
// text/template will call exported methods ({{.Foo}} can both get a field and call a method). So the template context
// as long as it can reach **any** type with exported methods, it is equivalent to exposing those methods to the template author.
// The context of this feature is deliberately all pure data (only exported fields, zero methods).
//
// If this fails: it means someone added a method to webhookTemplateData / webhookItem.
// Before deciding to allow it, first think clearly about whether that method can be used by the template to read things you don't want exposed.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d methods (%s): text/template can call them, "+
				"which is equivalent to opening up the capabilities of these methods to the template author", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal locks down the set of functions exposed to the template.
//
// Every extra function in the FuncMap adds one more capability. Currently there are only json / jsons, whose purpose is to serialize values
// into JSON fragments—cannot read files, cannot make requests, cannot execute commands.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("template function set changed: got %v, want %v. Before adding a function, please confirm it won't expand the capability surface"+
			" (cannot read/write files, cannot make network requests, cannot execute commands)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds access in the template:
// accessing something that doesn't exist must fail, rather than echoing something back; and the failure message must not leak internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a nonexistent field should error")
	}
	// The error must not contain the real content from the template context (vulnerability title/summary).
	for _, leak := range []string{"SQL injection", "parameter id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("template error leaked message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently a template typo is a configuration error, retrying won't self-heal.
// If it is judged retryable, one bad template will make every delivery waste three rounds of backoff.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("Template syntax errors should be caught at save time")
	}
	// Even if validation is bypassed and delivery is attempted directly, it must be judged a permanent failure rather than retried repeatedly.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("A bad template should be judged a permanent failure, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON covers the constraint that 'the template rendering result must be valid JSON'.
// It also incidentally blocks usages like 'using a template to generate plain text to trigger another protocol'.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// A valid template passes.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("A valid template should pass validation: %v", err)
	}
	// When non-JSON is rendered, it must be rejected (rather than sent out as-is).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("Rendering non-JSON should be judged a permanent failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("The error message should indicate it is a JSON problem, got %v", err)
	}
}
