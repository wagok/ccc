package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func blocksJSON(t *testing.T, md string) string {
	t.Helper()
	b, err := json.Marshal(markdownToRichBlocks(md))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// Plain prose gains nothing from a block tree, and sending one would swap a
// well-understood code path for a new one with no benefit.
func TestMarkdownToRichBlocksSkipsUnstructuredText(t *testing.T) {
	for _, md := range []string{
		"just a sentence",
		"two lines\nof plain prose",
		"",
		"a | b without a separator row is only punctuation",
	} {
		if got := markdownToRichBlocks(md); got != nil {
			t.Errorf("expected no blocks for %q, got %v", md, got)
		}
	}
}

func TestMarkdownToRichBlocksStructure(t *testing.T) {
	md := "# Title\n\nSome **bold** and `code`.\n\n" +
		"| Host | State |\n|---|---|\n| msi | up |\n\n" +
		"- one\n- two\n\n" +
		"> quoted\n\n---\n\n```go\nx := 1\n```"

	got := blocksJSON(t, md)
	for _, want := range []string{
		`"type":"heading"`,
		`"text":"bold","type":"bold"`,
		`"text":"code","type":"code"`,
		`"type":"table"`,
		`"is_bordered":true`,
		`"type":"list"`,
		`"type":"blockquote"`,
		`"type":"divider"`,
		`"type":"pre"`,
		`"language":"go"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	// The fence body must survive untouched — it is code, not prose.
	if !strings.Contains(got, `"text":"x := 1"`) {
		t.Errorf("code body altered:\n%s", got)
	}
}

func TestMarkdownTableCells(t *testing.T) {
	got := blocksJSON(t, "| a | b |\n| --- | --- |\n| 1 | 2 |")
	// Header plus one row, two cells each.
	if strings.Count(got, `"text":`) < 4 {
		t.Errorf("expected 4 cells, got:\n%s", got)
	}
	if !strings.Contains(got, `"text":"1"`) || !strings.Contains(got, `"text":"2"`) {
		t.Errorf("row cells missing:\n%s", got)
	}
}

func TestMarkdownOrderedList(t *testing.T) {
	got := blocksJSON(t, "1. first\n2. second")
	if !strings.Contains(got, `"is_ordered":true`) {
		t.Errorf("ordered flag missing:\n%s", got)
	}
	if !strings.Contains(got, `"first"`) || !strings.Contains(got, `"second"`) {
		t.Errorf("items missing:\n%s", got)
	}
}

func TestMarkdownInlineRuns(t *testing.T) {
	// A link only becomes a link with a real scheme; anything else stays text
	// rather than producing a URL Telegram would reject.
	got := blocksJSON(t, "# h\n\nsee [docs](https://x.dev) and [bad](notaurl)")
	if !strings.Contains(got, `"type":"url"`) || !strings.Contains(got, `"url":"https://x.dev"`) {
		t.Errorf("link not converted:\n%s", got)
	}
	if strings.Contains(got, `"url":"notaurl"`) {
		t.Errorf("schemeless link should stay plain text:\n%s", got)
	}
}

// A message that is Markdown on the way in and rendered on the way out should
// survive the round trip with its structure intact.
func TestMarkdownRoundTrip(t *testing.T) {
	md := "## Report\n\n| Host | State |\n| --- | --- |\n| msi | up |\n\n- one\n- two"

	blocks := markdownToRichBlocks(md)
	if blocks == nil {
		t.Fatal("expected structured blocks")
	}
	raw, _ := json.Marshal(map[string]interface{}{"blocks": blocks})
	back := richMessageText(raw)

	for _, want := range []string{"## Report", "| Host | State |", "| --- | --- |", "| msi | up |", "- one", "- two"} {
		if !strings.Contains(back, want) {
			t.Errorf("round trip lost %q:\n%s", want, back)
		}
	}
}

// Long answers are the normal case, so the splitter has to cut where Markdown
// survives it. splitMessage cuts on character count and will happily land in
// the middle of a table row or a code fence, leaving both halves unreadable.
func TestSplitMarkdownBlocksKeepsStructure(t *testing.T) {
	table := "| Host | State |\n| --- | --- |\n| msi | up |\n| dell | up |"
	long := strings.Repeat("Filler paragraph that pushes past the limit.\n\n", 30)
	text := long + table

	parts := splitMarkdownBlocks(text, 600)
	if len(parts) < 2 {
		t.Fatalf("expected the text to be split, got %d part(s)", len(parts))
	}
	// The table must live inside exactly one part, intact.
	whole := 0
	for _, p := range parts {
		if strings.Contains(p, "| Host | State |") {
			if !strings.Contains(p, "| dell | up |") {
				t.Errorf("table was cut across parts:\n%s", p)
			}
			whole++
		}
	}
	if whole != 1 {
		t.Errorf("table header appears in %d parts, want 1", whole)
	}
	if strings.Join(parts, "") == "" {
		t.Error("content lost")
	}
}

func TestSplitMarkdownBlocksKeepsFenceWhole(t *testing.T) {
	fence := "```go\n" + strings.Repeat("line of code\n", 20) + "```"
	text := strings.Repeat("prose paragraph\n\n", 20) + fence

	for _, p := range splitMarkdownBlocks(text, 500) {
		opens := strings.Count(p, "```")
		if opens%2 != 0 {
			t.Errorf("part has an unbalanced code fence (%d markers):\n%s", opens, p)
		}
	}
}

func TestSplitMarkdownBlocksShortTextUntouched(t *testing.T) {
	text := "## Title\n\nshort body"
	got := splitMarkdownBlocks(text, 4096)
	if len(got) != 1 || got[0] != text {
		t.Errorf("short text should pass through unchanged, got %q", got)
	}
}

// The common agent message is prose with a few emphases and identifiers and no
// headings or tables at all. Treating only block-level constructs as structure
// left exactly those messages showing their raw ** and ` markers.
func TestInlineOnlyTextStillConverts(t *testing.T) {
	for _, md := range []string{
		"Проверил **три величины** — сходятся.",
		"Смотри `handleHook` в main.go.",
		"Готово, см. [отчёт](https://x.dev).",
		"Это ~~не~~ важно.",
	} {
		blocks := markdownToRichBlocks(md)
		if blocks == nil {
			t.Errorf("inline formatting should convert, got nil for %q", md)
			continue
		}
		if len(blocks) != 1 || blocks[0]["type"] != "paragraph" {
			t.Errorf("expected one paragraph for %q, got %v", md, blocks)
		}
		if _, plain := blocks[0]["text"].(string); plain {
			t.Errorf("styling was flattened away for %q", md)
		}
	}
}

// Prose with no markers at all gains nothing from a block tree.
func TestTrulyPlainTextStaysPlain(t *testing.T) {
	for _, md := range []string{
		"Готово, проверил.",
		"Всё сходится: 302 809 помечено, over-reach 0.",
		"Отправил письмо агенту openarx и жду ответа.",
	} {
		if got := markdownToRichBlocks(md); got != nil {
			t.Errorf("plain prose should stay plain, got blocks for %q: %v", md, got)
		}
	}
}
