package main

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type DiskInfo struct {
	Drive   string
	TotalGB float64
	FreeGB  float64
	Usage   float64
}

type OracleSession struct {
	SID       string
	OSUser    string
	Machine   string
	SPID      string
	Cursors   float64
	LogonTime time.Time
}

type PageData struct {
	RenderTime time.Time

	Host         string
	OracleSource string

	DiskWarn   bool
	MemoryWarn bool

	Disks             []DiskInfo
	MemoryTotalGB     float64
	MemoryAvailableGB float64
	MemoryUsage       float64
	IISMemoryMB       float64
	FlyDocuments      float64

	OracleSessions []OracleSession
}

func (d *PageData) Empty() bool {
	return d.Host == "" && d.OracleSource == "" && len(d.Disks) == 0 &&
		d.MemoryTotalGB == 0 && d.IISMemoryMB == 0 && d.FlyDocuments == 0 &&
		len(d.OracleSessions) == 0 && d.RenderTime.IsZero()
}

var (
	pageTimeRe = regexp.MustCompile(`(\d{4})年(\d{2})月(\d{2})日【[^】]*】(\d{2}):(\d{2}):(\d{2})`)
	hostRe     = regexp.MustCompile(`当前：\s*([^\s<；;]+)\s*；`)
	diskLineRe = regexp.MustCompile(`([A-Za-z]:)\\?的总大小\(GB\)：([0-9.]+)，可用空间\(GB\)：([0-9.]+)，磁盘使用率：\s*([0-9.]+)\s*%`)
	memoryRe   = regexp.MustCompile(`内存总大小\(GB\)：([0-9.]+)，可用内存大小\(GB\)：([0-9.]+)，内存使用率：\s*([0-9.]+)\s*%`)
	flyDocRe   = regexp.MustCompile(`今日已生成飞行文件\s*([0-9]+)\s*份`)
	iisRe      = regexp.MustCompile(`IIS独占内存使用：\s*([\d,.]+)\s*MB`)
	numberRe   = regexp.MustCompile(`^[\d,]*\.?\d+$`)
	sourceRe   = regexp.MustCompile(`^\[([^\]]+)\]`)
)

var logonFormats = []string{
	"2006-01-02 15:04:05",
	"2006/01/02 15:04:05",
	"2006/1/2 15:04:05",
	"2006-01-02T15:04:05",
	time.RFC3339,
	"2006-01-02 15:04:05.0",
}

func ParsePage(body []byte) (*PageData, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	d := &PageData{}

	fullText := textOf(doc)

	if m := pageTimeRe.FindStringSubmatch(fullText); m != nil {
		d.RenderTime = time.Date(
			mustAtoi(m[1]), time.Month(mustAtoi(m[2])), mustAtoi(m[3]),
			mustAtoi(m[4]), mustAtoi(m[5]), mustAtoi(m[6]), 0, time.UTC)
	}
	if m := hostRe.FindStringSubmatch(fullText); m != nil {
		d.Host = m[1]
	}

	d.DiskWarn = strings.Contains(fullText, "硬盘使用不正常")
	d.MemoryWarn = strings.Contains(fullText, "内存使用不正常")

	if s := firstByIDSuffix(doc, "LabelDisk"); s != "" {
		for _, m := range diskLineRe.FindAllStringSubmatch(s, -1) {
			d.Disks = append(d.Disks, DiskInfo{
				Drive:   strings.ToUpper(m[1]),
				TotalGB: parseFloat(m[2]),
				FreeGB:  parseFloat(m[3]),
				Usage:   parseFloat(m[4]) / 100,
			})
		}
	}
	if s := firstByIDSuffix(doc, "LabelMemory"); s != "" {
		if m := memoryRe.FindStringSubmatch(s); m != nil {
			d.MemoryTotalGB = parseFloat(m[1])
			d.MemoryAvailableGB = parseFloat(m[2])
			d.MemoryUsage = parseFloat(m[3]) / 100
		}
	}

	iis := strings.TrimSpace(firstByIDSuffix(doc, "LabelMemoryIIS"))
	if numberRe.MatchString(iis) {
		d.IISMemoryMB = parseFloat(iis)
	} else if m := iisRe.FindStringSubmatch(fullText); m != nil {
		d.IISMemoryMB = parseFloat(strings.ReplaceAll(m[1], ",", ""))
	}

	fly := firstByIDSuffix(doc, "LabelFlyDocument")
	if fly == "" {
		fly = fullText
	}
	if m := flyDocRe.FindStringSubmatch(fly); m != nil {
		d.FlyDocuments = parseFloat(m[1])
	}

	if s := firstByIDSuffix(doc, "Label1"); s != "" {
		if m := sourceRe.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
			d.OracleSource = m[1]
		}
	}

	if table := findTable(doc, "table_connection"); table != nil {
		d.OracleSessions = parseSessions(table)
	}

	return d, nil
}

func parseSessions(table *html.Node) []OracleSession {
	var sessions []OracleSession
	for tr := range descendants(table, "tr") {
		var cells []string
		for td := range childElements(tr, "td") {
			cells = append(cells, strings.TrimSpace(textOf(td)))
		}
		if len(cells) < 7 {
			continue
		}
		s := OracleSession{
			SID:     cells[1],
			OSUser:  cells[2],
			Machine: cells[3],
			SPID:    cells[4],
			Cursors: parseFloat(strings.ReplaceAll(cells[5], ",", "")),
		}
		for _, f := range logonFormats {
			if t, err := time.ParseInLocation(f, cells[6], time.UTC); err == nil {
				s.LogonTime = t
				break
			}
		}
		sessions = append(sessions, s)
	}
	return sessions
}

func textOf(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			sb.WriteString(n.Data)
		case n.Type == html.ElementNode && n.Data == "br":
			sb.WriteString("\n")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

func firstByIDSuffix(root *html.Node, suffix string) string {
	var found string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != "" {
			return
		}
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "id" && strings.HasSuffix(a.Val, suffix) {
					found = textOf(n)
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return found
}

func findTable(root *html.Node, id string) *html.Node {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "table" {
			for _, a := range n.Attr {
				if a.Key == "id" && a.Val == id {
					found = n
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return found
}

func descendants(root *html.Node, tag string) func(func(*html.Node) bool) {
	return func(yield func(*html.Node) bool) {
		var walk func(*html.Node) bool
		walk = func(n *html.Node) bool {
			if n.Type == html.ElementNode && n.Data == tag {
				if !yield(n) {
					return false
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if !walk(c) {
					return false
				}
			}
			return true
		}
		walk(root)
	}
}

func childElements(n *html.Node, tag string) func(func(*html.Node) bool) {
	return func(yield func(*html.Node) bool) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.Data == tag {
				if !yield(c) {
					return
				}
			}
		}
	}
}

func parseFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if !numberRe.MatchString(s) {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	return v
}

func mustAtoi(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}
