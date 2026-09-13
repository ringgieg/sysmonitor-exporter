package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/html"
)

var (
	upDesc = prometheus.NewDesc(
		"sysmonitor_up",
		"Whether the target page was fetched and parsed successfully (1 = ok, 0 = failed).",
		[]string{"target"}, nil)
	scrapeErrorDesc = prometheus.NewDesc(
		"sysmonitor_scrape_error",
		"Set to 1 for the most recent scrape error of a target; absent when the scrape succeeded.",
		[]string{"target", "error"}, nil)
	scrapeDurationDesc = prometheus.NewDesc(
		"sysmonitor_scrape_duration_seconds",
		"Time spent fetching and parsing the target page.",
		[]string{"target"}, nil)
	infoDesc = prometheus.NewDesc(
		"sysmonitor_info",
		"Information about the monitored host, value is always 1.",
		[]string{"target", "host", "oracle_source"}, nil)
	pageRenderTimeDesc = prometheus.NewDesc(
		"sysmonitor_page_render_timestamp_seconds",
		"UTC timestamp embedded in the monitored page (page render time).",
		[]string{"target"}, nil)

	diskWarnDesc = prometheus.NewDesc(
		"sysmonitor_disk_warning",
		"1 if the page reports abnormal disk usage (any drive above 85%).",
		[]string{"target"}, nil)
	diskTotalDesc = prometheus.NewDesc(
		"sysmonitor_disk_total_bytes",
		"Total disk space in bytes as reported by the page.",
		[]string{"target", "drive"}, nil)
	diskFreeDesc = prometheus.NewDesc(
		"sysmonitor_disk_free_bytes",
		"Free disk space in bytes as reported by the page.",
		[]string{"target", "drive"}, nil)
	diskUsageDesc = prometheus.NewDesc(
		"sysmonitor_disk_usage_ratio",
		"Disk usage ratio (0-1) as reported by the page.",
		[]string{"target", "drive"}, nil)

	memoryWarnDesc = prometheus.NewDesc(
		"sysmonitor_memory_warning",
		"1 if the page reports abnormal memory usage (above 70%).",
		[]string{"target"}, nil)
	memoryTotalDesc = prometheus.NewDesc(
		"sysmonitor_memory_total_bytes",
		"Physical memory total in bytes as reported by the page.",
		[]string{"target"}, nil)
	memoryAvailableDesc = prometheus.NewDesc(
		"sysmonitor_memory_available_bytes",
		"Physical memory available in bytes as reported by the page.",
		[]string{"target"}, nil)
	memoryUsageDesc = prometheus.NewDesc(
		"sysmonitor_memory_usage_ratio",
		"Memory usage ratio (0-1) as reported by the page.",
		[]string{"target"}, nil)

	iisMemoryDesc = prometheus.NewDesc(
		"sysmonitor_iis_w3wp_memory_bytes",
		"Private memory of IIS worker process (w3wp.exe) in bytes; page flags values above 2048 MB.",
		[]string{"target"}, nil)
	flyDocsDesc = prometheus.NewDesc(
		"sysmonitor_fly_documents_today",
		"Number of flight documents generated today in D:\\flydocument.",
		[]string{"target"}, nil)

	oracleSessionsDesc = prometheus.NewDesc(
		"sysmonitor_oracle_sessions",
		"Number of Oracle sessions listed on the page (w3wp.exe sessions with open cursors).",
		[]string{"target"}, nil)
	sessionLabels = []string{"target", "sid", "osuser", "machine", "spid"}
	sessionCursorsDesc = prometheus.NewDesc(
		"sysmonitor_oracle_session_open_cursors",
		"Open cursors of the Oracle session; page flags values above 60.",
		sessionLabels, nil)
	sessionLogonTimeDesc = prometheus.NewDesc(
		"sysmonitor_oracle_session_logon_timestamp_seconds",
		"Logon time of the Oracle session as unix timestamp (UTC).",
		sessionLabels, nil)
	sessionLogonAgeDesc = prometheus.NewDesc(
		"sysmonitor_oracle_session_logon_age_seconds",
		"Seconds since the Oracle session logged on; page flags sessions older than 2 days.",
		sessionLabels, nil)
)

const (
	gibibyte = 1024 * 1024 * 1024
	mebibyte = 1024 * 1024
)

type Exporter struct {
	cfg     *Config
	clients map[string]*http.Client
	debug   bool

	mu    sync.Mutex
	cache map[string]cachedTarget
}

type cachedTarget struct {
	data      *PageData
	err       error
	duration  time.Duration
	fetchedAt time.Time
}

func NewExporter(cfg *Config, debug bool) *Exporter {
	clients := make(map[string]*http.Client, len(cfg.Targets))
	for _, t := range cfg.Targets {
		jar, _ := cookiejar.New(nil)
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: t.InsecureSkipVerify},
		}
		clients[t.Name] = &http.Client{
			Transport: transport,
			Timeout:   time.Duration(t.Timeout),
			Jar:       jar,
		}
	}
	return &Exporter{
		cfg:     cfg,
		clients: clients,
		debug:   debug,
		cache:   make(map[string]cachedTarget),
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		upDesc, scrapeErrorDesc, scrapeDurationDesc, infoDesc, pageRenderTimeDesc,
		diskWarnDesc, diskTotalDesc, diskFreeDesc, diskUsageDesc,
		memoryWarnDesc, memoryTotalDesc, memoryAvailableDesc, memoryUsageDesc,
		iisMemoryDesc, flyDocsDesc,
		oracleSessionsDesc, sessionCursorsDesc, sessionLogonTimeDesc, sessionLogonAgeDesc,
	} {
		ch <- d
	}
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	results := make(chan targetResult, len(e.cfg.Targets))
	var wg sync.WaitGroup
	for _, t := range e.cfg.Targets {
		wg.Add(1)
		go func(t TargetConfig) {
			defer wg.Done()
			data, dur, err := e.scrapeTarget(t)
			results <- targetResult{target: t.Name, data: data, duration: dur, err: err}
		}(t)
	}
	wg.Wait()
	close(results)

	for r := range results {
		e.emit(ch, r)
	}
}

type targetResult struct {
	target   string
	data     *PageData
	duration time.Duration
	err      error
}

func (e *Exporter) scrapeTarget(t TargetConfig) (*PageData, time.Duration, error) {
	e.mu.Lock()
	c, ok := e.cache[t.Name]
	e.mu.Unlock()
	if ok && e.cfg.CacheTTL > 0 && time.Since(c.fetchedAt) < time.Duration(e.cfg.CacheTTL) {
		return c.data, c.duration, c.err
	}

	start := time.Now()
	data, err := e.fetch(t)
	dur := time.Since(start)

	e.mu.Lock()
	e.cache[t.Name] = cachedTarget{data: data, err: err, duration: dur, fetchedAt: time.Now()}
	e.mu.Unlock()

	if err != nil {
		log.Printf("target %q (%s): scrape failed: %v", t.Name, t.URL, err)
	} else if e.debug {
		log.Printf("target %q (%s): ok in %.3fs: host=%s oracle=%s disks=%d mem=%.2f/%.2fGB iis=%.1fMB fly=%.0f sessions=%d",
			t.Name, t.URL, dur.Seconds(), data.Host, data.OracleSource, len(data.Disks),
			data.MemoryAvailableGB, data.MemoryTotalGB, data.IISMemoryMB, data.FlyDocuments, len(data.OracleSessions))
	}
	return data, dur, err
}

func (e *Exporter) fetch(t TargetConfig) (*PageData, error) {
	status, body, finalURL, err := e.get(t)
	if err != nil {
		return nil, err
	}
	if isLoginPage(body) {
		if t.Username == "" {
			return nil, fmt.Errorf("site requires forms authentication (got login.aspx); set username and password for this target")
		}
		log.Printf("target %q: forms authentication detected, logging in as %q", t.Name, t.Username)
		if err := e.formsLogin(t, body, finalURL); err != nil {
			return nil, fmt.Errorf("forms login: %w", err)
		}
		status, body, _, err = e.get(t)
		if err != nil {
			return nil, err
		}
		if isLoginPage(body) {
			return nil, fmt.Errorf("forms login failed for target %q: still on login page after posting credentials (wrong username/password?)", t.Name)
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", status, t.URL)
	}
	data, err := ParsePage(body)
	if err != nil {
		return nil, err
	}
	if data.Empty() {
		msg := fmt.Sprintf("page contains no monitor fields (status %d, %d bytes, body starts: %s)", status, len(body), bodyPreview(body))
		if dump := dumpBody(t.Name, body); dump != "" {
			msg += fmt.Sprintf(" [full body dumped to %s]", dump)
		}
		log.Printf("target %q (%s): %s", t.Name, t.URL, msg)
		return nil, errors.New(msg)
	}
	return data, nil
}

func (e *Exporter) get(t TargetConfig) (int, []byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, t.URL, nil)
	if err != nil {
		return 0, nil, "", err
	}
	if t.Username != "" {
		req.SetBasicAuth(t.Username, t.Password)
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(t.Timeout))
	defer cancel()

	if e.debug {
		log.Printf("target %q: GET %s (timeout %s)", t.Name, t.URL, time.Duration(t.Timeout))
	}
	resp, err := e.clients[t.Name].Do(req.WithContext(ctx))
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	var reader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return 0, nil, "", fmt.Errorf("decompress gzip body: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	body, err := io.ReadAll(io.LimitReader(reader, 8<<20))
	if err != nil {
		return 0, nil, "", err
	}
	if e.debug {
		log.Printf("target %q: status %s, body %d bytes", t.Name, resp.Status, len(body))
	}
	return resp.StatusCode, body, resp.Request.URL.String(), nil
}

func isLoginPage(body []byte) bool {
	return bytes.Contains(body, []byte("TextBox_Password")) &&
		(bytes.Contains(body, []byte("Button_Login")) || bytes.Contains(body, []byte("login.aspx")))
}

func (e *Exporter) formsLogin(t TargetConfig, loginPage []byte, pageURL string) error {
	doc, err := html.Parse(bytes.NewReader(loginPage))
	if err != nil {
		return fmt.Errorf("parse login page: %w", err)
	}
	var action string
	fields := url.Values{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "form":
				for _, a := range n.Attr {
					if a.Key == "action" {
						action = a.Val
					}
				}
			case "input":
				var name, value string
				for _, a := range n.Attr {
					switch a.Key {
					case "name":
						name = a.Val
					case "value":
						value = a.Val
					}
				}
				if name != "" {
					fields.Set(name, value)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if action == "" {
		return fmt.Errorf("no form found on login page")
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return err
	}
	ref, err := url.Parse(action)
	if err != nil {
		return err
	}
	loginURL := base.ResolveReference(ref).String()

	fields.Set("TextBox_UserName", t.Username)
	fields.Set("TextBox_Password", t.Password)
	fields.Set("CheckBox_RememberMe", "on")
	if fields.Get("Button_Login") == "" {
		fields.Set("Button_Login", "登录")
	}

	req, err := http.NewRequest(http.MethodPost, loginURL, strings.NewReader(fields.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if e.debug {
		log.Printf("target %q: POST %s (login)", t.Name, loginURL)
	}
	resp, err := e.clients[t.Name].Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return nil
}

func (e *Exporter) emit(ch chan<- prometheus.Metric, r targetResult) {
	up := 1.0
	if r.err != nil {
		up = 0
	}
	ch <- prometheus.MustNewConstMetric(upDesc, prometheus.GaugeValue, up, r.target)
	ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, r.duration.Seconds(), r.target)
	if r.err != nil {
		ch <- prometheus.MustNewConstMetric(scrapeErrorDesc, prometheus.GaugeValue, 1, r.target, sanitizeLabel(r.err.Error()))
		return
	}

	d := r.data
	ch <- prometheus.MustNewConstMetric(infoDesc, prometheus.GaugeValue, 1, r.target, d.Host, d.OracleSource)
	if !d.RenderTime.IsZero() {
		ch <- prometheus.MustNewConstMetric(pageRenderTimeDesc, prometheus.GaugeValue, float64(d.RenderTime.Unix()), r.target)
	}

	diskWarn := 0.0
	if d.DiskWarn {
		diskWarn = 1
	}
	ch <- prometheus.MustNewConstMetric(diskWarnDesc, prometheus.GaugeValue, diskWarn, r.target)
	for _, disk := range d.Disks {
		ch <- prometheus.MustNewConstMetric(diskTotalDesc, prometheus.GaugeValue, disk.TotalGB*gibibyte, r.target, disk.Drive)
		ch <- prometheus.MustNewConstMetric(diskFreeDesc, prometheus.GaugeValue, disk.FreeGB*gibibyte, r.target, disk.Drive)
		ch <- prometheus.MustNewConstMetric(diskUsageDesc, prometheus.GaugeValue, disk.Usage, r.target, disk.Drive)
	}

	memoryWarn := 0.0
	if d.MemoryWarn {
		memoryWarn = 1
	}
	ch <- prometheus.MustNewConstMetric(memoryWarnDesc, prometheus.GaugeValue, memoryWarn, r.target)
	ch <- prometheus.MustNewConstMetric(memoryTotalDesc, prometheus.GaugeValue, d.MemoryTotalGB*gibibyte, r.target)
	ch <- prometheus.MustNewConstMetric(memoryAvailableDesc, prometheus.GaugeValue, d.MemoryAvailableGB*gibibyte, r.target)
	ch <- prometheus.MustNewConstMetric(memoryUsageDesc, prometheus.GaugeValue, d.MemoryUsage, r.target)

	ch <- prometheus.MustNewConstMetric(iisMemoryDesc, prometheus.GaugeValue, d.IISMemoryMB*mebibyte, r.target)
	ch <- prometheus.MustNewConstMetric(flyDocsDesc, prometheus.GaugeValue, d.FlyDocuments, r.target)

	ch <- prometheus.MustNewConstMetric(oracleSessionsDesc, prometheus.GaugeValue, float64(len(d.OracleSessions)), r.target)
	now := time.Now()
	for _, s := range d.OracleSessions {
		labels := []string{r.target, s.SID, s.OSUser, s.Machine, s.SPID}
		ch <- prometheus.MustNewConstMetric(sessionCursorsDesc, prometheus.GaugeValue, s.Cursors, labels...)
		if !s.LogonTime.IsZero() {
			ch <- prometheus.MustNewConstMetric(sessionLogonTimeDesc, prometheus.GaugeValue, float64(s.LogonTime.Unix()), labels...)
			ch <- prometheus.MustNewConstMetric(sessionLogonAgeDesc, prometheus.GaugeValue, now.Sub(s.LogonTime).Seconds(), labels...)
		}
	}
}

func sanitizeLabel(s string) string {
	r := strings.NewReplacer("\n", " ", "\r", " ", `"`, "'")
	s = strings.Join(strings.Fields(r.Replace(s)), " ")
	const maxLen = 180
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return s
}

func bodyPreview(body []byte) string {
	const n = 160
	if len(body) > n {
		body = body[:n]
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, string(body))
	if !utf8.ValidString(s) {
		return "0x" + hex.EncodeToString(body)
	}
	return strconv.Quote(s)
}

func dumpBody(target string, body []byte) string {
	name := fmt.Sprintf("sysmonitor-dump-%s.htm", sanitizeFileName(target))
	if err := os.WriteFile(name, body, 0o644); err != nil {
		log.Printf("cannot dump response body to %s: %v", name, err)
		return ""
	}
	return name
}

func sanitizeFileName(s string) string {
	r := strings.NewReplacer(
		`\`, "_", `/`, "_", `:`, "_", `*`, "_", `?`, "_",
		`"`, "_", `<`, "_", `>`, "_", `|`, "_", " ", "_",
	)
	return r.Replace(s)
}
