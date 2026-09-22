package main

// rich_outbound.go converts an agent's Markdown into Telegram's rich-message
// block tree (Bot API 10.1) so structure survives the trip to the human.
//
// Until now CCC sent every message with no parse_mode at all, so an agent's
// Markdown arrived as literal ** and | characters — a table was unreadable and
// a code block was indistinguishable from prose.
//
// The parser deliberately covers only the subset agents actually emit:
// headings, fenced code, tables, lists, block quotes, dividers and inline
// bold/italic/code/strikethrough/links. A full CommonMark implementation would
// bring edge-case interpretations nobody asked for; anything unrecognised stays
// a plain paragraph, which is exactly what it looks like today.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// richBlock is one node of the outgoing block tree.
type richBlock map[string]interface{}

var (
	reMDHeading = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reMDFence   = regexp.MustCompile("^```([A-Za-z0-9_+-]*)\\s*$")
	reMDBullet  = regexp.MustCompile(`^\s*[-*+]\s+(.*)$`)
	reMDOrdered = regexp.MustCompile(`^\s*\d+[.)]\s+(.*)$`)
	reMDQuote   = regexp.MustCompile(`^>\s?(.*)$`)
	reMDRule    = regexp.MustCompile(`^\s*(-{3,}|\*{3,}|_{3,})\s*$`)
	reMDSepRow  = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)

	// Inline runs, longest markers first so ** is not read as two *.
	reInline = regexp.MustCompile("(`[^`]+`)|(\\*\\*[^*]+\\*\\*)|(~~[^~]+~~)|(\\*[^*]+\\*)|(_[^_]+_)|(\\[[^\\]]+\\]\\([^)]+\\))")
)

// mdInline renders one line of Markdown into a rich text node: a bare string
// when there is no styling, else a list of runs.
func mdInline(line string) interface{} {
	locs := reInline.FindAllStringIndex(line, -1)
	if len(locs) == 0 {
		return line
	}
	var runs []interface{}
	pos := 0
	for _, loc := range locs {
		if loc[0] > pos {
			runs = append(runs, line[pos:loc[0]])
		}
		tok := line[loc[0]:loc[1]]
		switch {
		case strings.HasPrefix(tok, "`"):
			runs = append(runs, richBlock{"type": "code", "text": strings.Trim(tok, "`")})
		case strings.HasPrefix(tok, "**"):
			runs = append(runs, richBlock{"type": "bold", "text": strings.Trim(tok, "*")})
		case strings.HasPrefix(tok, "~~"):
			runs = append(runs, richBlock{"type": "strikethrough", "text": strings.Trim(tok, "~")})
		case strings.HasPrefix(tok, "["):
			if i := strings.Index(tok, "]("); i > 0 {
				text, href := tok[1:i], strings.TrimSuffix(tok[i+2:], ")")
				if u, err := url.Parse(href); err == nil && u.Scheme != "" {
					runs = append(runs, richBlock{"type": "url", "text": text, "url": href})
				} else {
					runs = append(runs, tok)
				}
			} else {
				runs = append(runs, tok)
			}
		default: // *italic* or _italic_
			runs = append(runs, richBlock{"type": "italic", "text": strings.Trim(tok, "*_")})
		}
		pos = loc[1]
	}
	if pos < len(line) {
		runs = append(runs, line[pos:])
	}
	if len(runs) == 1 {
		return runs[0]
	}
	return runs
}

// splitTableRow splits "| a | b |" into its cells.
func splitTableRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	var cells []string
	for _, c := range strings.Split(line, "|") {
		cells = append(cells, strings.TrimSpace(strings.ReplaceAll(c, `\|`, "|")))
	}
	return cells
}

// markdownToRichBlocks converts Markdown into a block tree. It returns nil when
// the text carries no structure worth sending as a rich message — plain prose
// is better served by an ordinary message than by a block tree.
func markdownToRichBlocks(text string) []richBlock {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var blocks []richBlock
	var para []string
	structured := false

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		joined := strings.TrimSpace(strings.Join(para, "\n"))
		para = nil
		if joined == "" {
			return
		}
		blocks = append(blocks, richBlock{"type": "paragraph", "text": mdInline(joined)})
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			flushPara()
			continue
		}

		if m := reMDFence.FindStringSubmatch(trimmed); m != nil {
			flushPara()
			var body []string
			i++
			for ; i < len(lines); i++ {
				if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
					break
				}
				body = append(body, lines[i])
			}
			b := richBlock{"type": "pre", "text": strings.Join(body, "\n")}
			if m[1] != "" {
				b["language"] = m[1]
			}
			blocks = append(blocks, b)
			structured = true
			continue
		}

		if reMDRule.MatchString(trimmed) {
			flushPara()
			blocks = append(blocks, richBlock{"type": "divider"})
			structured = true
			continue
		}

		if m := reMDHeading.FindStringSubmatch(trimmed); m != nil {
			flushPara()
			size := len(m[1])
			if size > 6 {
				size = 6
			}
			blocks = append(blocks, richBlock{"type": "heading", "size": size, "text": mdInline(strings.TrimSpace(m[2]))})
			structured = true
			continue
		}

		// A table needs its separator row to be a table at all; without one the
		// pipes are just punctuation in a sentence.
		if strings.Contains(trimmed, "|") && i+1 < len(lines) && reMDSepRow.MatchString(lines[i+1]) {
			flushPara()
			header := splitTableRow(trimmed)
			var cells []interface{}
			var head []interface{}
			for _, h := range header {
				head = append(head, richBlock{"text": mdInline(h)})
			}
			cells = append(cells, head)
			i += 2
			for ; i < len(lines); i++ {
				row := strings.TrimSpace(lines[i])
				if !strings.Contains(row, "|") || row == "" {
					i--
					break
				}
				var r []interface{}
				for _, c := range splitTableRow(row) {
					r = append(r, richBlock{"text": mdInline(c)})
				}
				cells = append(cells, r)
			}
			blocks = append(blocks, richBlock{"type": "table", "is_bordered": true, "cells": cells})
			structured = true
			continue
		}

		if m := reMDQuote.FindStringSubmatch(trimmed); m != nil {
			flushPara()
			quote := []string{m[1]}
			for i+1 < len(lines) {
				n := reMDQuote.FindStringSubmatch(strings.TrimSpace(lines[i+1]))
				if n == nil {
					break
				}
				quote = append(quote, n[1])
				i++
			}
			blocks = append(blocks, richBlock{"type": "blockquote", "blocks": []richBlock{
				{"type": "paragraph", "text": mdInline(strings.Join(quote, "\n"))},
			}})
			structured = true
			continue
		}

		bullet := reMDBullet.FindStringSubmatch(line)
		ordered := reMDOrdered.FindStringSubmatch(line)
		if bullet != nil || ordered != nil {
			flushPara()
			isOrdered := ordered != nil
			var items []interface{}
			for i < len(lines) {
				l := lines[i]
				var item string
				if isOrdered {
					m := reMDOrdered.FindStringSubmatch(l)
					if m == nil {
						break
					}
					item = m[1]
				} else {
					m := reMDBullet.FindStringSubmatch(l)
					if m == nil {
						break
					}
					item = m[1]
				}
				items = append(items, richBlock{"blocks": []richBlock{
					{"type": "paragraph", "text": mdInline(strings.TrimSpace(item))},
				}})
				i++
			}
			i--
			b := richBlock{"type": "list", "items": items}
			if isOrdered {
				b["is_ordered"] = true
			}
			blocks = append(blocks, b)
			structured = true
			continue
		}

		para = append(para, line)
	}
	flushPara()

	if !structured || len(blocks) == 0 {
		return nil // nothing a block tree would render better than plain text
	}
	// Telegram rejects a message whose blocks render to nothing (a lone divider
	// is the case that actually happens), so fall back to plain text there.
	renders := false
	for _, b := range blocks {
		if b["type"] != "divider" {
			renders = true
			break
		}
	}
	if !renders {
		return nil
	}
	return blocks
}

// richParams builds the sendRichMessage/editMessageText parameters for a block
// tree, or reports false when the text has no structure worth a rich message.
func richParams(text string) (string, bool) {
	blocks := markdownToRichBlocks(text)
	if blocks == nil {
		return "", false
	}
	richJSON, err := json.Marshal(map[string]interface{}{"blocks": blocks})
	if err != nil {
		return "", false
	}
	return string(richJSON), true
}

// splitMarkdownBlocks cuts long Markdown at block boundaries so each piece is
// still valid Markdown. splitMessage cuts on character count, which can land
// inside a table row or a code fence and leave both halves unreadable.
func splitMarkdownBlocks(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	// Paragraph breaks are the coarsest safe seam; a fenced code block must not
	// be cut at one, so fences are tracked and their interior kept whole.
	var parts []string
	var cur strings.Builder
	inFence := false

	flush := func() {
		if cur.Len() > 0 {
			parts = append(parts, strings.TrimRight(cur.String(), "\n"))
			cur.Reset()
		}
	}

	for _, para := range strings.Split(text, "\n\n") {
		if strings.Count(para, "```")%2 == 1 {
			inFence = !inFence
		}
		candidate := para + "\n\n"
		if !inFence && cur.Len() > 0 && cur.Len()+len(candidate) > maxLen {
			flush()
		}
		cur.WriteString(candidate)
	}
	flush()

	// A single block over the limit cannot be split safely by structure; fall
	// back to the character splitter for that piece alone.
	var out []string
	for _, p := range parts {
		if len(p) <= maxLen {
			out = append(out, p)
			continue
		}
		out = append(out, splitMessage(p, maxLen)...)
	}
	return out
}

// sendRichOrPlain delivers text as a rich message when it has structure worth
// preserving, and as an ordinary message otherwise. Any failure falls back to
// plain text: a formatting problem must never cost the human the message.
func sendRichOrPlain(config *Config, chatID, threadID int64, text string) error {
	for _, chunk := range splitMarkdownBlocks(text, telegramTextLimit) {
		if err := sendOneRichOrPlain(config, chatID, threadID, chunk); err != nil {
			return err
		}
	}
	return nil
}

func sendOneRichOrPlain(config *Config, chatID, threadID int64, text string) error {
	richJSON, ok := richParams(text)
	if !ok {
		return sendMessage(config, chatID, threadID, text)
	}
	params := url.Values{
		"chat_id":      {fmt.Sprintf("%d", chatID)},
		"rich_message": {richJSON},
	}
	if threadID > 0 {
		params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
	}
	result, err := telegramAPI(config, "sendRichMessage", params)
	if err != nil || result == nil || !result.OK {
		fmt.Fprintf(os.Stderr, "[rich] sendRichMessage failed (%s) — falling back to plain text\n", richFailReason(result, err))
		return sendMessage(config, chatID, threadID, text)
	}
	return nil
}

// editRichOrPlain replaces a streamed message with its final text, as a rich
// message when the text has structure. Converting only at finalize is safe in a
// way per-delta conversion is not: the text is complete, so its Markdown is
// valid. A failure leaves the already-streamed plain text in place.
func editRichOrPlain(cfg *Config, chatID int64, msgID int, text string) {
	richJSON, ok := richParams(text)
	if !ok {
		editStreamMessage(cfg, chatID, msgID, text)
		return
	}
	result, err := telegramAPI(cfg, "editMessageText", url.Values{
		"chat_id":      {fmt.Sprintf("%d", chatID)},
		"message_id":   {fmt.Sprintf("%d", msgID)},
		"rich_message": {richJSON},
	})
	if err != nil || result == nil || !result.OK {
		fmt.Fprintf(os.Stderr, "[rich] editMessageText failed (%s) — keeping plain text\n", richFailReason(result, err))
		editStreamMessage(cfg, chatID, msgID, text)
	}
}

func richFailReason(result *TelegramResponse, err error) string {
	if err != nil {
		return err.Error()
	}
	if result != nil && result.Description != "" {
		return result.Description
	}
	return "unknown"
}
