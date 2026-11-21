package folderkb

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/myjnathan/agentgate/internal/store"
)

// block is the intermediate form every extractor produces: headings,
// text runs and explicit page breaks. assemble() turns blocks into
// pages, a section tree and per-node text.
type block struct {
	kind  int // blockText | blockHeading | blockPageBreak
	level int // heading level (1-3)
	title string
	text  string
}

const (
	blockText = iota
	blockHeading
	blockPageBreak
)

// parseDocument extracts one file into a Document: title, pages,
// section tree and flattened sections. The title is the first H1
// for Markdown, otherwise the file name without its extension.
func parseDocument(src *store.KBFolderSource, rel string, b []byte) (*Document, error) {
	ext := strings.ToLower(filepath.Ext(rel))
	title := strings.TrimSuffix(filepath.Base(rel), ext)
	var blocks []block
	var err error
	switch ext {
	case ".md":
		var h1 string
		blocks, h1, err = extractText(b, true)
		if h1 != "" {
			title = h1
		}
	case ".txt":
		blocks, _, err = extractText(b, false)
	case ".docx":
		var docTitle string
		blocks, docTitle, err = extractDocx(b)
		if docTitle != "" {
			title = docTitle
		}
	case ".pdf":
		blocks, err = extractPDF(b)
	default:
		return nil, fmt.Errorf("unsupported extension %q", ext)
	}
	if err != nil {
		return nil, err
	}
	doc := assemble(title, blocks)
	doc.ID = DocID(src.ID, rel)
	doc.SourceID = src.ID
	doc.RelPath = rel
	doc.Level = src.Level
	return doc, nil
}

// ---- assembly: blocks -> pages, tree, sections ----

type headingInfo struct {
	level int
	title string
	page  int
	index int // index into the block list
}

// assemble paginates the blocks (45 lines per pseudo-page, explicit
// page breaks honoured), builds the section tree from the headings,
// and flattens the tree into sections. A node's own text is the text
// between its heading and the next heading of any level — child
// sections are not duplicated into their parent. The root node (n0)
// holds the preface text before the first heading; its section text
// is the whole document, so a short document can be read in one call.
// A heading-less document with more than one page gets one child
// node per page ("第 N 页") so citations stay page-precise instead
// of pointing at the whole document.
func assemble(title string, blocks []block) *Document {
	type pageRun struct {
		page int
		text string
	}
	type placed struct {
		b         block
		startPage int
		endPage   int
		runs      []pageRun // text blocks only: lines grouped by page
	}
	page := 1
	linesInPage := 0
	var placedBlocks []placed
	var headings []headingInfo
	for _, b := range blocks {
		switch b.kind {
		case blockPageBreak:
			page++
			linesInPage = 0
		case blockHeading:
			if linesInPage >= LinesPerPage {
				page++
				linesInPage = 0
			}
			headings = append(headings, headingInfo{level: b.level, title: b.title, page: page, index: len(placedBlocks)})
			placedBlocks = append(placedBlocks, placed{b: b, startPage: page, endPage: page})
			linesInPage++
		case blockText:
			lines := strings.Split(b.text, "\n")
			start := page
			var runs []pageRun
			curPage := page
			var cur strings.Builder
			for _, ln := range lines {
				if linesInPage >= LinesPerPage {
					page++
					linesInPage = 0
				}
				if curPage != page {
					runs = append(runs, pageRun{page: curPage, text: cur.String()})
					cur.Reset()
					curPage = page
				}
				if cur.Len() > 0 {
					cur.WriteString("\n")
				}
				cur.WriteString(ln)
				linesInPage++
			}
			runs = append(runs, pageRun{page: curPage, text: cur.String()})
			placedBlocks = append(placedBlocks, placed{b: b, startPage: start, endPage: page, runs: runs})
		}
	}
	pages := 1
	for _, p := range placedBlocks {
		if p.endPage > pages {
			pages = p.endPage
		}
	}

	// Own text of a heading = text blocks after it, up to the next
	// heading block (any level). Root own text = text before the
	// first heading.
	ownText := func(startIdx, endIdx int) string {
		var parts []string
		for i := startIdx; i < endIdx && i < len(placedBlocks); i++ {
			if placedBlocks[i].b.kind == blockText {
				parts = append(parts, placedBlocks[i].b.text)
			}
		}
		return strings.Join(parts, "\n")
	}
	headingEnd := func(i int) int {
		if i+1 < len(headings) {
			return headings[i+1].index
		}
		return len(placedBlocks)
	}
	// Page of the last text line inside a span (for EndPage).
	lastTextPage := func(startIdx, endIdx int) int {
		last := -1
		for i := startIdx; i < endIdx && i < len(placedBlocks); i++ {
			if placedBlocks[i].b.kind == blockText {
				last = placedBlocks[i].endPage
			}
		}
		return last
	}

	root := &Node{ID: "n0", Title: title, StartPage: 1, EndPage: pages}
	// Stack of (node, level) for tree building; root is level 0.
	type stackEnt struct {
		n     *Node
		level int
	}
	stack := []stackEnt{{n: root, level: 0}}
	// Track each node's span for its own text.
	type span struct {
		node                  *Node
		startIdx, endIdx      int
		headingPage, lastPage int
	}
	var spans []span
	rootEnd := len(placedBlocks)
	if len(headings) > 0 {
		rootEnd = headings[0].index
	}
	spans = append(spans, span{node: root, startIdx: 0, endIdx: rootEnd, headingPage: 1, lastPage: lastTextPage(0, rootEnd)})
	for i, h := range headings {
		n := &Node{ID: fmt.Sprintf("n%d", i+1), Title: h.title, StartPage: h.page, EndPage: h.page}
		for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1].n
		parent.Children = append(parent.Children, n)
		stack = append(stack, stackEnt{n: n, level: h.level})
		spans = append(spans, span{node: n, startIdx: h.index + 1, endIdx: headingEnd(i), headingPage: h.page, lastPage: lastTextPage(h.index+1, headingEnd(i))})
	}
	// Per-page nodes for heading-less multi-page documents.
	texts := map[*Node]string{}
	for _, sp := range spans {
		texts[sp.node] = ownText(sp.startIdx, sp.endIdx)
	}
	if len(headings) == 0 && pages > 1 {
		for p := 1; p <= pages; p++ {
			var parts []string
			for _, pb := range placedBlocks {
				for _, r := range pb.runs {
					if r.page == p {
						parts = append(parts, r.text)
					}
				}
			}
			text := strings.Join(parts, "\n")
			n := &Node{ID: fmt.Sprintf("n%d", p), Title: fmt.Sprintf("第 %d 页", p),
				StartPage: p, EndPage: p, Summary: summarize(text)}
			root.Children = append(root.Children, n)
			texts[n] = text
		}
	}
	// EndPage = max(own last text page, children's end pages),
	// computed bottom-up (spans are in pre-order, so reverse order
	// visits children before parents).
	for i := len(spans) - 1; i >= 0; i-- {
		sp := spans[i]
		end := sp.headingPage
		if sp.lastPage > end {
			end = sp.lastPage
		}
		for _, ch := range sp.node.Children {
			if ch.EndPage > end {
				end = ch.EndPage
			}
		}
		sp.node.EndPage = end
	}
	// Summaries: a heading with no own text borrows the first
	// sentence of its first descendant's text so the tree stays useful.
	for i, sp := range spans {
		if sp.node.Summary != "" {
			continue
		}
		sum := summarize(texts[sp.node])
		if sum == "" {
			for j := i + 1; j < len(spans); j++ {
				if t := texts[spans[j].node]; strings.TrimSpace(t) != "" {
					sum = summarize(t)
					break
				}
			}
		}
		sp.node.Summary = sum
	}
	// Sections in pre-order; the root section carries the whole
	// document text so a short document reads in one call.
	var allText []string
	for _, p := range placedBlocks {
		if p.b.kind == blockText {
			allText = append(allText, p.b.text)
		}
	}
	fullText := strings.Join(allText, "\n")
	var sections []*Section
	var walk func(n *Node)
	walk = func(n *Node) {
		text := texts[n]
		if n == root {
			text = fullText
		}
		sections = append(sections, &Section{
			NodeID: n.ID, Title: n.Title,
			StartPage: n.StartPage, EndPage: n.EndPage, Text: text,
		})
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(root)
	return &Document{Title: title, Pages: pages, Tree: root, sections: sections}
}

// ---- plain text / Markdown ----

var mdHeading = regexp.MustCompile(`^(#{1,3})\s+(.+?)\s*#*\s*$`)

// extractText parses .txt/.md into blocks. For Markdown, ATX
// headings (#/##/###) become tree nodes; the first H1 that appears
// before any text is the document title (returned separately) and
// does not become a tree node — it *is* the document.
func extractText(b []byte, markdown bool) (blocks []block, h1 string, err error) {
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	var buf []string
	flush := func() {
		if len(buf) == 0 {
			return
		}
		t := strings.Join(buf, "\n")
		if strings.TrimSpace(t) != "" {
			blocks = append(blocks, block{kind: blockText, text: t})
		}
		buf = nil
	}
	for _, line := range strings.Split(text, "\n") {
		if markdown {
			if m := mdHeading.FindStringSubmatch(line); m != nil {
				flush()
				level := len(m[1])
				if level == 1 && h1 == "" && len(blocks) == 0 {
					h1 = m[2]
					continue
				}
				blocks = append(blocks, block{kind: blockHeading, level: level, title: m[2]})
				continue
			}
		}
		buf = append(buf, line)
	}
	flush()
	return blocks, h1, nil
}

// ---- .docx (zip + XML, stdlib only) ----

// extractDocx reads word/document.xml: paragraphs become text
// blocks, Heading1/2/3 paragraph styles become tree nodes, and
// explicit page breaks become page breaks. The first Heading1
// before any content is the document title (returned separately).
// Only w:t character data becomes text (w:instrText field codes and
// w:delText deletions are ignored); runs are concatenated and tabs kept.
func extractDocx(b []byte) ([]block, string, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, "", fmt.Errorf("docx: %w", err)
	}
	var docXML []byte
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return nil, "", fmt.Errorf("docx: %w", err)
			}
			docXML, err = io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return nil, "", fmt.Errorf("docx: %w", err)
			}
			break
		}
	}
	if docXML == nil {
		return nil, "", fmt.Errorf("docx: word/document.xml not found")
	}
	dec := xml.NewDecoder(bytes.NewReader(docXML))
	var blocks []block
	var stack []string
	var para strings.Builder
	paraStyle := ""
	pageBreak := false
	inPara := false
	title := ""
	sawContent := false
	flushPara := func() {
		if !inPara {
			return
		}
		t := para.String()
		if pageBreak {
			blocks = append(blocks, block{kind: blockPageBreak})
		}
		if lvl, ok := docxHeadingLevel(paraStyle); ok && strings.TrimSpace(t) != "" {
			if lvl == 1 && title == "" && !sawContent {
				title = strings.TrimSpace(t)
			} else {
				blocks = append(blocks, block{kind: blockHeading, level: lvl, title: strings.TrimSpace(t)})
				sawContent = true
			}
		} else if strings.TrimSpace(t) != "" {
			blocks = append(blocks, block{kind: blockText, text: t})
			sawContent = true
		}
		para.Reset()
		paraStyle = ""
		pageBreak = false
		inPara = false
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("docx: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			switch t.Name.Local {
			case "p":
				flushPara()
				inPara = true
			case "pStyle":
				for _, a := range t.Attr {
					if a.Name.Local == "val" {
						paraStyle = a.Value
					}
				}
			case "br":
				for _, a := range t.Attr {
					if a.Name.Local == "type" && a.Value == "page" {
						pageBreak = true
					}
				}
			case "tab":
				if inPara {
					para.WriteString("\t")
				}
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if t.Name.Local == "p" {
				flushPara()
			}
		case xml.CharData:
			if inPara && len(stack) > 0 && stack[len(stack)-1] == "t" {
				para.Write([]byte(t))
			}
		}
	}
	flushPara()
	return blocks, title, nil
}

// docxHeadingLevel maps a paragraph style to a heading level:
// "Heading1" (and localized "标题 1") -> 1, and so on to 3.
func docxHeadingLevel(style string) (int, bool) {
	s := strings.ReplaceAll(style, " ", "")
	switch s {
	case "Heading1", "标题1":
		return 1, true
	case "Heading2", "标题2":
		return 2, true
	case "Heading3", "标题3":
		return 3, true
	}
	return 0, false
}

// ---- .pdf (best-effort text extraction, stdlib only) ----

// extractPDF pulls text out of a PDF's content streams: each stream
// is Flate-decoded when its dictionary says so, and literal strings
// shown by Tj/TJ operators are collected in order. When the number
// of text-bearing streams equals the page count, streams map 1:1 to
// real pages; otherwise all text is concatenated and pseudo-paged.
// Scanned-image PDFs have no text to extract and return an error.
func extractPDF(b []byte) ([]block, error) {
	pages := countPDFPages(b)
	streams := findPDFStreams(b)
	var texts []string
	for _, s := range streams {
		data := s.data
		if s.flate {
			zr, err := zlib.NewReader(bytes.NewReader(data))
			if err != nil {
				continue
			}
			raw, err := io.ReadAll(zr)
			_ = zr.Close()
			if err != nil {
				continue
			}
			data = raw
		}
		if !bytes.Contains(data, []byte("BT")) && !bytes.Contains(data, []byte("Tj")) && !bytes.Contains(data, []byte("TJ")) {
			continue
		}
		if t := extractPDFText(data); strings.TrimSpace(t) != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		return nil, fmt.Errorf("pdf: no extractable text (scanned image or custom font encoding?)")
	}
	if pages > 0 && len(texts) == pages {
		var blocks []block
		for i, t := range texts {
			if i > 0 {
				blocks = append(blocks, block{kind: blockPageBreak})
			}
			blocks = append(blocks, block{kind: blockText, text: t})
		}
		return blocks, nil
	}
	return []block{{kind: blockText, text: strings.Join(texts, "\n")}}, nil
}

type pdfStream struct {
	data  []byte
	flate bool
}

// countPDFPages counts "/Type /Page" entries ("/Type /Pages" — the
// tree node — is excluded by the trailing-character check).
func countPDFPages(b []byte) int {
	n := 0
	for i := 0; i+5 < len(b); i++ {
		if b[i] != '/' || string(b[i+1:i+5]) != "Type" {
			continue
		}
		j := i + 5
		for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\r' || b[j] == '\n') {
			j++
		}
		if j+5 <= len(b) && string(b[j:j+5]) == "/Page" {
			after := byte(0)
			if j+5 < len(b) {
				after = b[j+5]
			}
			if after != 's' {
				n++
			}
		}
	}
	return n
}

// findPDFStreams returns every stream's raw bytes plus whether its
// dictionary (the ~256 bytes before "stream") names FlateDecode.
func findPDFStreams(b []byte) []pdfStream {
	var out []pdfStream
	for i := 0; i+6 < len(b); i++ {
		if string(b[i:i+6]) != "stream" {
			continue
		}
		// "stream" must be followed by CRLF or LF (not "streaming" etc).
		j := i + 6
		if j < len(b) && b[j] == '\r' {
			j++
		}
		if j < len(b) && b[j] == '\n' {
			j++
		} else {
			continue
		}
		start := j
		end := bytes.Index(b[start:], []byte("endstream"))
		if end < 0 {
			break
		}
		data := b[start : start+end]
		// Trim the newline that precedes "endstream" (part of the
		// delimiter, not of the stream).
		data = bytes.TrimSuffix(data, []byte("\r\n"))
		data = bytes.TrimSuffix(data, []byte("\n"))
		dictStart := i - 256
		if dictStart < 0 {
			dictStart = 0
		}
		flate := bytes.Contains(b[dictStart:i], []byte("FlateDecode"))
		out = append(out, pdfStream{data: data, flate: flate})
		i = start + end
	}
	return out
}

// extractPDFText collects the literal strings of a content stream
// in order, unescaping PDF escapes (including octal \ddd).
func extractPDFText(data []byte) string {
	var parts []string
	for i := 0; i < len(data); i++ {
		if data[i] != '(' {
			continue
		}
		var sb strings.Builder
		depth := 1
		i++
		closed := false
		for ; i < len(data); i++ {
			c := data[i]
			if c == '\\' && i+1 < len(data) {
				n := data[i+1]
				switch {
				case n == 'n':
					sb.WriteByte('\n')
					i++
				case n == 'r':
					sb.WriteByte('\r')
					i++
				case n == 't':
					sb.WriteByte('\t')
					i++
				case n == 'b':
					sb.WriteByte('\b')
					i++
				case n == 'f':
					sb.WriteByte('\f')
					i++
				case n == '(' || n == ')' || n == '\\':
					sb.WriteByte(n)
					i++
				case n >= '0' && n <= '7':
					v := 0
					k := 0
					for k < 3 && i+1+k < len(data) && data[i+1+k] >= '0' && data[i+1+k] <= '7' {
						v = v*8 + int(data[i+1+k]-'0')
						k++
					}
					sb.WriteByte(byte(v))
					i += k
				default:
					// Backslash + line break = continuation; any
					// other escaped byte is taken literally.
					if n != '\r' && n != '\n' {
						sb.WriteByte(n)
					}
					i++
				}
				continue
			}
			if c == '(' {
				depth++
				sb.WriteByte(c)
				continue
			}
			if c == ')' {
				depth--
				if depth == 0 {
					closed = true
					break
				}
				sb.WriteByte(c)
				continue
			}
			sb.WriteByte(c)
		}
		if closed {
			if s := sb.String(); strings.TrimSpace(s) != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, " ")
}
