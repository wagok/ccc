package main

import (
	"strings"
	"testing"
)

// Verbatim rich_message from the Hetzner maintenance notice CCC dropped on
// 2026-09-22: paragraphs, a styled run, and a bordered table — and no "text"
// field anywhere, which is why the message read as empty and vanished.
const hetznerRichMessage = `{"blocks":[
 {"type":"paragraph","text":"hetzner прислал письмо:"},
 {"type":"paragraph","text":{"type":"bold","text":"Maintenance on Storage Box hosts FSN1-BX2001 to FSN1-BX2400"}},
 {"type":"paragraph","text":"We will perform maintenance work on the Storage Box hosts."},
 {"type":"table","is_bordered":true,"cells":[
   [{"text":{"type":"bold","text":"Type:"},"align":"center"},{"text":"Maintenance","align":"center"}],
   [{"text":{"type":"bold","text":"State:"},"align":"center"},{"text":"planned","align":"center"}],
   [{"text":{"type":"bold","text":"Estimated end:"},"align":"center"},{"text":"2026-10-01, 10:00 AM UTC","align":"center"}]
 ]}
]}`

func TestRichMessageText(t *testing.T) {
	got := richMessageText([]byte(hetznerRichMessage))

	for _, want := range []string{
		"hetzner прислал письмо:",
		"**Maintenance on Storage Box hosts FSN1-BX2001 to FSN1-BX2400**", // styled run -> Markdown
		"| **Type:** | Maintenance |",                                     // real table row
		"| --- | --- |",                                                   // header separator
		"| **Estimated end:** | 2026-10-01, 10:00 AM UTC |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in rendered output:\n%s", want, got)
		}
	}
}

// Every block type an agent would care about has to survive the trip, because
// the structure IS the information — a table flattened to prose stops being a
// table the agent can read back.
func TestRichMessageBlockKinds(t *testing.T) {
	raw := `{"blocks":[
	  {"type":"section_heading","text":"Report"},
	  {"type":"paragraph","text":"intro"},
	  {"type":"preformatted","language":"go","text":"x := **1**"},
	  {"type":"list","items":["one","two"]},
	  {"type":"list","is_ordered":true,"items":["first","second"]},
	  {"type":"block_quotation","text":"quoted"},
	  {"type":"divider"},
	  {"type":"details","title":"More","text":"hidden"}
	]}`
	got := richMessageText([]byte(raw))

	for _, want := range []string{
		"## Report",
		"```go\nx := **1**\n```", // code stays verbatim: no Markdown applied inside
		"- one",
		"1. first",
		"> quoted",
		"---",
		"<details><summary>More</summary>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// A pipe inside a cell would silently split the column it sits in.
func TestRichMessageTableEscapesPipes(t *testing.T) {
	raw := `{"blocks":[{"type":"table","cells":[[{"text":"a|b"},{"text":"c"}]]}]}`
	got := richMessageText([]byte(raw))
	if !strings.Contains(got, `a\|b`) {
		t.Errorf("pipe not escaped in cell:\n%s", got)
	}
}

func TestRichTextRunNesting(t *testing.T) {
	if got := richTextRun("plain"); got != "plain" {
		t.Errorf("string: %q", got)
	}
	if got := richTextRun(map[string]interface{}{"type": "bold", "text": "loud"}); got != "**loud**" {
		t.Errorf("styled: %q", got)
	}
	// Styling nests, so the markers have to nest with it.
	nested := map[string]interface{}{"type": "bold", "text": map[string]interface{}{"type": "italic", "text": "deep"}}
	if got := richTextRun(nested); got != "**_deep_**" {
		t.Errorf("nested styling: %q", got)
	}
	mixed := []interface{}{"a", map[string]interface{}{"type": "code", "text": "b"}, "c"}
	if got := richTextRun(mixed); got != "a`b`c" {
		t.Errorf("mixed run: %q", got)
	}
	link := map[string]interface{}{"type": "text_link", "text": "docs", "url": "https://x.dev"}
	if got := richTextRun(link); got != "[docs](https://x.dev)" {
		t.Errorf("link: %q", got)
	}
	// A style Markdown cannot express degrades to its text rather than to
	// invented syntax the agent would have to guess at.
	spoiler := map[string]interface{}{"type": "spoiler", "text": "shh"}
	if got := richTextRun(spoiler); got != "shh" {
		t.Errorf("unsupported style should degrade to plain text, got %q", got)
	}
	if got := richTextRun(map[string]interface{}{"type": "image"}); got != "" {
		t.Errorf("node without text should be empty, got %q", got)
	}
}

func TestRichMessageTextEmptyInputs(t *testing.T) {
	if got := richMessageText(nil); got != "" {
		t.Errorf("nil: %q", got)
	}
	if got := richMessageText([]byte(`not json`)); got != "" {
		t.Errorf("malformed JSON must not panic or leak, got %q", got)
	}
	if got := richMessageText([]byte(`{"blocks":[]}`)); got != "" {
		t.Errorf("no blocks: %q", got)
	}
}
