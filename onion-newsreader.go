package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	M2UsenetURL  = "http://itcxzfm2h36hfj6j7qxksyfm4ipp3co4rkl62sgge7hp6u77lbretiyd.onion:8880"
	TorProxyAddr = "127.0.0.1:9050"
	PrimaryNNTP  = "peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion:119"
	FallbackNNTP = "news.tcpreset.net:119"
)

type NewsgroupInfo struct {
	Name   string
	High   int
	Low    int
	Status string
}

type ArticleInfo struct {
	Number       int
	Subject      string
	From         string
	Date         string
	MessageID    string
	References   string   // References header for threading
	ParentID     string   // Direct parent Message-ID
	ReplySubject string   // For m2usenet reply link
	ReplyRef     string   // For m2usenet reply link
	Depth        int      // Threading depth (0 = root)
	Children     []*ArticleInfo // Child posts (replies)
}

type NNTPClient struct {
	conn            net.Conn
	reader          *bufio.Reader
	mutex           sync.Mutex
	isConnected     bool
	connectedServer string
	lastUsed        time.Time
}

type NewsServer struct {
	client  *NNTPClient
	groups  map[string]*NewsgroupInfo
	mutex   sync.RWMutex
	session string
	downloadStatus struct {
		IsDownloading bool
		StartTime     time.Time
		LastError     string
		Completed     bool
		GroupCount    int
		mutex         sync.RWMutex
	}
}

func NewNNTPClient() *NNTPClient {
	return &NNTPClient{}
}

func NewNewsServer() *NewsServer {
	sessionBytes := make([]byte, 8)
	rand.Read(sessionBytes)
	return &NewsServer{
		client:  NewNNTPClient(),
		groups:  make(map[string]*NewsgroupInfo),
		session: fmt.Sprintf("%x", sessionBytes),
	}
}

// ============= SOCKS5 / Tor Connection =============

func (c *NNTPClient) connectViaSocks5(targetAddr string) (net.Conn, error) {
	proxyConn, err := net.DialTimeout("tcp", TorProxyAddr, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("tor proxy unreachable: %v", err)
	}

	deadline := 60 * time.Second
	if strings.Contains(targetAddr, ".onion") {
		deadline = 180 * time.Second
	}
	proxyConn.SetDeadline(time.Now().Add(deadline))
	defer proxyConn.SetDeadline(time.Time{})

	if _, err := proxyConn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("socks5 auth failed: %v", err)
	}

	authResp := make([]byte, 2)
	if _, err := io.ReadFull(proxyConn, authResp); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("socks5 auth response failed: %v", err)
	}
	if authResp[0] != 0x05 || authResp[1] != 0x00 {
		proxyConn.Close()
		return nil, fmt.Errorf("socks5 auth rejected")
	}

	parts := strings.Split(targetAddr, ":")
	host := parts[0]
	port, _ := strconv.Atoi(parts[1])

	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, []byte(host)...)
	req = append(req, byte(port>>8), byte(port&0xff))

	if _, err := proxyConn.Write(req); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("socks5 connect failed: %v", err)
	}

	resp := make([]byte, 4)
	if _, err := io.ReadFull(proxyConn, resp); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("socks5 response failed: %v", err)
	}

	if resp[1] != 0x00 {
		proxyConn.Close()
		errCodes := map[byte]string{
			0x01: "general failure", 0x02: "not allowed", 0x03: "network unreachable",
			0x04: "host unreachable", 0x05: "connection refused", 0x06: "TTL expired",
		}
		if msg, ok := errCodes[resp[1]]; ok {
			return nil, fmt.Errorf("socks5: %s", msg)
		}
		return nil, fmt.Errorf("socks5 error: %d", resp[1])
	}

	switch resp[3] {
	case 0x01:
		io.ReadFull(proxyConn, make([]byte, 6))
	case 0x03:
		lenByte := make([]byte, 1)
		io.ReadFull(proxyConn, lenByte)
		io.ReadFull(proxyConn, make([]byte, lenByte[0]+2))
	case 0x04:
		io.ReadFull(proxyConn, make([]byte, 18))
	}

	return proxyConn, nil
}

func (c *NNTPClient) connect() error {
	targets := []struct {
		addr string
		name string
	}{
		{PrimaryNNTP, "Onion Service"},
		{FallbackNNTP, "Fallback (via Tor)"},
	}

	var lastErr error
	for _, target := range targets {
		log.Printf("🔗 Connecting to %s...", target.name)

		conn, err := c.connectViaSocks5(target.addr)
		if err != nil {
			lastErr = err
			log.Printf("❌ %s failed: %v", target.name, err)
			continue
		}

		c.conn = conn
		c.reader = bufio.NewReader(conn)

		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		welcome, err := c.reader.ReadString('\n')
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			conn.Close()
			lastErr = err
			continue
		}
		log.Printf("📥 Welcome: %s", strings.TrimSpace(welcome))

		if _, err := conn.Write([]byte("MODE READER\r\n")); err != nil {
			conn.Close()
			lastErr = err
			continue
		}

		modeResp, err := c.reader.ReadString('\n')
		if err != nil {
			conn.Close()
			lastErr = err
			continue
		}

		if !strings.HasPrefix(modeResp, "200") && !strings.HasPrefix(modeResp, "201") {
			conn.Close()
			lastErr = fmt.Errorf("MODE READER failed: %s", modeResp)
			continue
		}

		c.isConnected = true
		c.connectedServer = target.name
		c.lastUsed = time.Now()
		log.Printf("✅ Connected to %s", target.name)
		return nil
	}

	return fmt.Errorf("all servers failed: %v", lastErr)
}

func (c *NNTPClient) ensureConnected() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if !c.isConnected || c.conn == nil || time.Since(c.lastUsed) > 5*time.Minute {
		if c.conn != nil {
			c.conn.Close()
		}
		c.isConnected = false
		return c.connect()
	}
	c.lastUsed = time.Now()
	return nil
}

func (c *NNTPClient) ListGroups() (map[string]*NewsgroupInfo, error) {
	if err := c.ensureConnected(); err != nil {
		return nil, err
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	log.Printf("📤 Sending LIST command...")
	if _, err := c.conn.Write([]byte("LIST\r\n")); err != nil {
		return nil, err
	}

	resp, err := c.reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(resp, "215") {
		return nil, fmt.Errorf("LIST failed: %s", resp)
	}

	groups := make(map[string]*NewsgroupInfo)
	lineCount := 0

	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("error reading groups at line %d: %v", lineCount, err)
		}
		line = strings.TrimSpace(line)
		if line == "." {
			break
		}

		lineCount++
		if lineCount%5000 == 0 {
			log.Printf("📊 Read %d groups...", lineCount)
		}

		parts := strings.Fields(line)
		if len(parts) >= 4 {
			high, _ := strconv.Atoi(parts[1])
			low, _ := strconv.Atoi(parts[2])
			groups[parts[0]] = &NewsgroupInfo{
				Name:   parts[0],
				High:   high,
				Low:    low,
				Status: parts[3],
			}
		}
	}

	log.Printf("✅ Loaded %d groups total", len(groups))
	return groups, nil
}

func (c *NNTPClient) SelectGroup(name string) (int, int, int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if !c.isConnected {
		return 0, 0, 0, fmt.Errorf("not connected")
	}

	if _, err := c.conn.Write([]byte("GROUP " + name + "\r\n")); err != nil {
		return 0, 0, 0, err
	}

	resp, err := c.reader.ReadString('\n')
	if err != nil {
		return 0, 0, 0, err
	}

	if !strings.HasPrefix(resp, "211") {
		return 0, 0, 0, fmt.Errorf("GROUP failed: %s", resp)
	}

	parts := strings.Fields(resp)
	if len(parts) < 4 {
		return 0, 0, 0, fmt.Errorf("invalid response")
	}

	count, _ := strconv.Atoi(parts[1])
	low, _ := strconv.Atoi(parts[2])
	high, _ := strconv.Atoi(parts[3])
	return count, low, high, nil
}

func (c *NNTPClient) GetRecentArticles(low, high, max int) ([]*ArticleInfo, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if !c.isConnected {
		return nil, fmt.Errorf("not connected")
	}

	if high-low+1 > max {
		low = high - max + 1
	}

	cmd := fmt.Sprintf("XOVER %d-%d\r\n", low, high)
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return nil, err
	}

	resp, err := c.reader.ReadString('\n')
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(resp, "224") {
		return nil, fmt.Errorf("XOVER failed: %s", resp)
	}

	var articles []*ArticleInfo
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "." {
			break
		}

		parts := strings.Split(line, "\t")
		if len(parts) >= 5 {
			num, _ := strconv.Atoi(parts[0])
			date := ""
			if len(parts) > 3 {
				date = parts[3]
			}
			// XOVER format: num subject from date message-id references bytes lines
			refs := ""
			if len(parts) > 5 {
				refs = strings.TrimSpace(parts[5])
			}
			// Extract parent ID (last message-id in references)
			parentID := ""
			if refs != "" {
				// References contains space-separated message-ids
				refParts := strings.Fields(refs)
				if len(refParts) > 0 {
					parentID = refParts[len(refParts)-1]
				}
			}
			articles = append(articles, &ArticleInfo{
				Number:     num,
				Subject:    parts[1],
				From:       parts[2],
				Date:       date,
				MessageID:  strings.TrimSpace(parts[4]),
				References: refs,
				ParentID:   parentID,
			})
		}
	}

	return articles, nil
}

func (c *NNTPClient) GetArticle(articleNum int) (map[string]string, string, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if !c.isConnected {
		return nil, "", fmt.Errorf("not connected")
	}

	cmd := fmt.Sprintf("ARTICLE %d\r\n", articleNum)
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return nil, "", err
	}

	resp, err := c.reader.ReadString('\n')
	if err != nil {
		return nil, "", err
	}

	if !strings.HasPrefix(resp, "220") {
		return nil, "", fmt.Errorf("ARTICLE failed: %s", resp)
	}

	headers := make(map[string]string)
	var bodyLines []string
	inHeaders := true
	currentHeader := ""

	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, "", err
		}
		line = strings.TrimRight(line, "\r\n")

		if line == "." {
			break
		}

		// Handle dot-stuffing
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}

		if inHeaders {
			if line == "" {
				inHeaders = false
				continue
			}

			// Continuation of previous header
			if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && currentHeader != "" {
				headers[currentHeader] += " " + strings.TrimSpace(line)
				continue
			}

			// New header
			if idx := strings.Index(line, ":"); idx > 0 {
				currentHeader = line[:idx]
				headers[currentHeader] = strings.TrimSpace(line[idx+1:])
			}
		} else {
			bodyLines = append(bodyLines, line)
		}
	}

	return headers, strings.Join(bodyLines, "\n"), nil
}

func (c *NNTPClient) Close() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.conn != nil {
		c.conn.Write([]byte("QUIT\r\n"))
		c.conn.Close()
	}
	c.isConnected = false
}

// ============= Server Handlers =============

func (s *NewsServer) testTorProxy() error {
	conn, err := net.DialTimeout("tcp", TorProxyAddr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}

	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}

	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("socks5 failed")
	}

	return nil
}

func (s *NewsServer) searchGroups(query string) []*NewsgroupInfo {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if query == "" {
		return nil
	}

	pattern := strings.ToLower(query)
	pattern = strings.ReplaceAll(pattern, ".", "\\.")
	pattern = strings.ReplaceAll(pattern, "*", ".*")
	pattern = strings.ReplaceAll(pattern, "?", ".")

	if !strings.Contains(query, "*") && !strings.Contains(query, "?") {
		pattern = ".*" + pattern + ".*"
	}

	regex, err := regexp.Compile("^" + pattern + "$")
	if err != nil {
		var results []*NewsgroupInfo
		queryLower := strings.ToLower(query)
		for _, g := range s.groups {
			if strings.Contains(strings.ToLower(g.Name), queryLower) {
				results = append(results, g)
			}
		}
		return results
	}

	var results []*NewsgroupInfo
	for _, g := range s.groups {
		if regex.MatchString(strings.ToLower(g.Name)) {
			results = append(results, g)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	return results
}

func (s *NewsServer) handleHome(w http.ResponseWriter, r *http.Request) {
	s.mutex.RLock()
	groupCount := len(s.groups)
	s.mutex.RUnlock()

	torOK := s.testTorProxy() == nil

	tmpl := template.Must(template.New("home").Parse(homeTemplate))
	tmpl.Execute(w, map[string]interface{}{
		"GroupCount":  groupCount,
		"TorOK":       torOK,
		"Session":     s.session,
		"M2UsenetURL": M2UsenetURL,
	})
}

func (s *NewsServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	s.downloadStatus.mutex.Lock()
	s.downloadStatus.IsDownloading = true
	s.downloadStatus.StartTime = time.Now()
	s.downloadStatus.LastError = ""
	s.downloadStatus.Completed = false
	s.downloadStatus.GroupCount = 0
	s.downloadStatus.mutex.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		done := make(chan error, 1)
		go func() {
			groups, err := s.client.ListGroups()
			if err != nil {
				done <- err
				return
			}
			s.mutex.Lock()
			s.groups = groups
			s.mutex.Unlock()

			s.downloadStatus.mutex.Lock()
			s.downloadStatus.GroupCount = len(groups)
			s.downloadStatus.mutex.Unlock()
			done <- nil
		}()

		select {
		case err := <-done:
			s.downloadStatus.mutex.Lock()
			s.downloadStatus.IsDownloading = false
			if err != nil {
				s.downloadStatus.LastError = err.Error()
			} else {
				s.downloadStatus.Completed = true
			}
			s.downloadStatus.mutex.Unlock()
		case <-ctx.Done():
			s.downloadStatus.mutex.Lock()
			s.downloadStatus.IsDownloading = false
			s.downloadStatus.LastError = "timeout after 10 minutes"
			s.downloadStatus.mutex.Unlock()
		}
	}()

	tmpl := template.Must(template.New("download").Parse(downloadingTemplate))
	tmpl.Execute(w, nil)
}

func (s *NewsServer) handleDownloadStatus(w http.ResponseWriter, r *http.Request) {
	s.downloadStatus.mutex.RLock()
	defer s.downloadStatus.mutex.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"downloading":%t,"completed":%t,"groups":%d,"error":"%s","elapsed":%d}`,
		s.downloadStatus.IsDownloading,
		s.downloadStatus.Completed,
		s.downloadStatus.GroupCount,
		s.downloadStatus.LastError,
		int(time.Since(s.downloadStatus.StartTime).Seconds()))
}

func (s *NewsServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	var results []*NewsgroupInfo
	if query != "" {
		results = s.searchGroups(query)
	}

	truncated := false
	if len(results) > 500 {
		results = results[:500]
		truncated = true
	}

	s.mutex.RLock()
	totalGroups := len(s.groups)
	s.mutex.RUnlock()

	tmpl := template.Must(template.New("search").Parse(searchTemplate))
	tmpl.Execute(w, map[string]interface{}{
		"Query":       query,
		"Results":     results,
		"ResultCount": len(results),
		"TotalGroups": totalGroups,
		"Truncated":   truncated,
		"M2UsenetURL": M2UsenetURL,
	})
}

// buildThreads organizes articles into a threaded structure
func buildThreads(articles []*ArticleInfo) []*ArticleInfo {
	if len(articles) == 0 {
		return articles
	}

	// Create map of MessageID -> Article
	byMsgID := make(map[string]*ArticleInfo)
	for _, art := range articles {
		msgID := art.MessageID
		// Normalize message-id (ensure <> brackets)
		if !strings.HasPrefix(msgID, "<") {
			msgID = "<" + msgID
		}
		if !strings.HasSuffix(msgID, ">") {
			msgID = msgID + ">"
		}
		byMsgID[msgID] = art
	}

	// Build parent-child relationships
	var roots []*ArticleInfo
	for _, art := range articles {
		parentID := art.ParentID
		// Normalize parent ID
		if parentID != "" {
			if !strings.HasPrefix(parentID, "<") {
				parentID = "<" + parentID
			}
			if !strings.HasSuffix(parentID, ">") {
				parentID = parentID + ">"
			}
		}

		if parentID == "" {
			// No parent - this is a root thread
			roots = append(roots, art)
		} else if parent, ok := byMsgID[parentID]; ok {
			// Parent found in our set - add as child
			parent.Children = append(parent.Children, art)
		} else {
			// Parent not in our set - treat as root
			roots = append(roots, art)
		}
	}

	// Sort roots by article number (newest first)
	sort.Slice(roots, func(i, j int) bool {
		return roots[i].Number > roots[j].Number
	})

	// Flatten the tree with depth information
	var result []*ArticleInfo
	var flatten func(art *ArticleInfo, depth int)
	flatten = func(art *ArticleInfo, depth int) {
		art.Depth = depth
		result = append(result, art)
		// Sort children by article number (oldest first within thread)
		sort.Slice(art.Children, func(i, j int) bool {
			return art.Children[i].Number < art.Children[j].Number
		})
		for _, child := range art.Children {
			flatten(child, depth+1)
		}
	}

	for _, root := range roots {
		flatten(root, 0)
	}

	return result
}

func (s *NewsServer) handleGroup(w http.ResponseWriter, r *http.Request) {
	groupName := strings.TrimPrefix(r.URL.Path, "/group/")
	if groupName == "" {
		http.Redirect(w, r, "/search", http.StatusFound)
		return
	}

	var articles []*ArticleInfo
	var connErr string

	if err := s.client.ensureConnected(); err != nil {
		connErr = err.Error()
	} else {
		_, low, high, err := s.client.SelectGroup(groupName)
		if err != nil {
			connErr = err.Error()
		} else {
			articles, err = s.client.GetRecentArticles(low, high, 100) // Get more for threading
			if err != nil {
				connErr = err.Error()
			}
		}
	}

	// Compute ReplySubject and ReplyRef for each article
	for _, art := range articles {
		// Subject: add "Re: " only if not already present
		subj := art.Subject
		if !strings.HasPrefix(strings.ToLower(subj), "re:") {
			subj = "Re: " + subj
		}
		art.ReplySubject = subj

		// MessageID: ensure <> brackets
		msgID := art.MessageID
		if msgID != "" {
			if !strings.HasPrefix(msgID, "<") {
				msgID = "<" + msgID
			}
			if !strings.HasSuffix(msgID, ">") {
				msgID = msgID + ">"
			}
		}
		art.ReplyRef = msgID
	}

	// Build threaded view
	articles = buildThreads(articles)

	tmpl := template.Must(template.New("group").Parse(groupTemplate))
	tmpl.Execute(w, map[string]interface{}{
		"GroupName":   groupName,
		"Articles":    articles,
		"Error":       connErr,
		"M2UsenetURL": M2UsenetURL,
	})
}

func (s *NewsServer) handleArticle(w http.ResponseWriter, r *http.Request) {
	// URL format: /article/group.name/12345
	path := strings.TrimPrefix(r.URL.Path, "/article/")
	parts := strings.Split(path, "/")

	if len(parts) < 2 {
		http.Redirect(w, r, "/search", http.StatusFound)
		return
	}

	groupName := strings.Join(parts[:len(parts)-1], "/")
	articleNum, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		http.Error(w, "Invalid article number", http.StatusBadRequest)
		return
	}

	var headers map[string]string
	var body string
	var connErr string

	if err := s.client.ensureConnected(); err != nil {
		connErr = err.Error()
	} else {
		_, _, _, err := s.client.SelectGroup(groupName)
		if err != nil {
			connErr = err.Error()
		} else {
			headers, body, err = s.client.GetArticle(articleNum)
			if err != nil {
				connErr = err.Error()
			}
		}
	}

	// Build reply URL
	replyURL := ""
	if headers != nil {
		subject := headers["Subject"]
		if subject != "" && !strings.HasPrefix(strings.ToLower(subject), "re:") {
			subject = "Re: " + subject
		}
		msgID := headers["Message-ID"]
		if msgID != "" && !strings.HasPrefix(msgID, "<") {
			msgID = "<" + msgID + ">"
		}

		replyURL = fmt.Sprintf("%s?newsgroups=%s&subject=%s&references=%s",
			M2UsenetURL,
			url.QueryEscape(groupName),
			url.QueryEscape(subject),
			url.QueryEscape(msgID))
	}

	tmpl := template.Must(template.New("article").Parse(articleTemplate))
	tmpl.Execute(w, map[string]interface{}{
		"GroupName":   groupName,
		"ArticleNum":  articleNum,
		"Headers":     headers,
		"Body":        body,
		"Error":       connErr,
		"ReplyURL":    replyURL,
		"M2UsenetURL": M2UsenetURL,
	})
}

func (s *NewsServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	torOK := s.testTorProxy() == nil

	var nntpStatus, nntpServer string
	if err := s.client.ensureConnected(); err != nil {
		nntpStatus = "disconnected"
	} else {
		nntpStatus = "connected"
		nntpServer = s.client.connectedServer
	}

	s.mutex.RLock()
	groupCount := len(s.groups)
	s.mutex.RUnlock()

	tmpl := template.Must(template.New("health").Parse(healthTemplate))
	tmpl.Execute(w, map[string]interface{}{
		"TorOK":      torOK,
		"NNTPStatus": nntpStatus,
		"NNTPServer": nntpServer,
		"GroupCount": groupCount,
		"Session":    s.session,
		"Timestamp":  time.Now().Format("2006-01-02 15:04:05 UTC"),
	})
}

// ============= Templates =============

const homeTemplate = `<!DOCTYPE html>
<html>
<head>
    <title>🧅 Onion Newsreader</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #0f0; padding: 20px; }
        .container { max-width: 800px; margin: 0 auto; }
        .box { background: #1a1a1a; padding: 20px; border-radius: 8px; margin: 15px 0; border: 1px solid #333; }
        .btn { display: inline-block; background: #333; color: #0f0; padding: 12px 24px; border-radius: 4px; text-decoration: none; margin: 5px; border: none; cursor: pointer; font-family: monospace; }
        .btn:hover { background: #555; }
        .btn-primary { background: #004400; border: 1px solid #0f0; }
        .status-ok { color: #0f0; }
        .status-err { color: #f80; }
        a { color: #0af; text-decoration: none; }
        a:hover { color: #0f0; }
        h1 { color: #0f0; }
    </style>
</head>
<body>
    <div class="container">
        <h1>🧅 Onion Newsreader</h1>
        <p>Minimal NNTP client via Tor</p>

        <div class="box">
            <h3>📊 Status</h3>
            <p>Tor Proxy: {{if .TorOK}}<span class="status-ok">✅ OK</span>{{else}}<span class="status-err">❌ Failed</span>{{end}}</p>
            <p>Cached Groups: <strong>{{.GroupCount}}</strong></p>
            <p>Session: <code>{{.Session}}</code></p>
        </div>

        <div class="box">
            <h3>🔧 Actions</h3>
            <a href="/download" class="btn btn-primary">📥 Download Active File</a>
            <a href="/search" class="btn">🔍 Search Groups</a>
            <a href="/health" class="btn">🔍 Health Check</a>
        </div>

        <div class="box">
            <h3>✍️ Post New Message</h3>
            <p>Create a new post (opens m2usenet):</p>
            <a href="{{.M2UsenetURL}}" target="_blank" class="btn btn-primary">📝 New Post</a>
        </div>

        <div class="box">
            <h3>ℹ️ Info</h3>
            <p>Primary: <code>peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion</code></p>
            <p>Fallback: <code>news.tcpreset.net</code> (via Tor)</p>
            <p>Posting: <code>{{.M2UsenetURL}}</code></p>
        </div>
    </div>
</body>
</html>`

const downloadingTemplate = `<!DOCTYPE html>
<html>
<head>
    <title>⏳ Downloading...</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #0f0; padding: 20px; text-align: center; }
        .box { background: #1a1a1a; padding: 30px; border-radius: 8px; max-width: 500px; margin: 50px auto; border: 1px solid #f80; }
        .spinner { animation: spin 1s linear infinite; display: inline-block; font-size: 2em; }
        @keyframes spin { 100% { transform: rotate(360deg); } }
        a { color: #0af; }
        #status { margin: 20px 0; padding: 15px; background: #0a0a0a; border-radius: 4px; }
    </style>
</head>
<body>
    <div class="box">
        <h2><span class="spinner">🔄</span> Downloading Active File</h2>
        <p>This may take 2-10 minutes via Tor...</p>
        <div id="status">⏳ Starting...</div>
        <p><a href="/">← Back to Home</a></p>
    </div>
    <script>
        function checkStatus() {
            fetch('/download-status')
                .then(r => r.json())
                .then(d => {
                    const el = document.getElementById('status');
                    if (d.completed) {
                        el.innerHTML = '✅ Done! ' + d.groups + ' groups loaded';
                        el.style.color = '#0f0';
                        setTimeout(() => location.href = '/search', 2000);
                    } else if (d.error) {
                        el.innerHTML = '❌ Error: ' + d.error;
                        el.style.color = '#f00';
                    } else if (d.downloading) {
                        el.innerHTML = '⏳ Downloading... (' + d.elapsed + 's) - ' + d.groups + ' groups so far';
                    }
                });
        }
        setInterval(checkStatus, 2000);
        checkStatus();
    </script>
</body>
</html>`

const searchTemplate = `<!DOCTYPE html>
<html>
<head>
    <title>🔍 Search Groups</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #0f0; padding: 20px; }
        .container { max-width: 900px; margin: 0 auto; }
        .search-box { background: #1a1a1a; padding: 20px; border-radius: 8px; margin-bottom: 20px; border: 1px solid #333; }
        .search-input { width: 70%; padding: 12px; background: #0a0a0a; border: 1px solid #333; color: #0f0; font-family: monospace; font-size: 1em; }
        .search-btn { padding: 12px 24px; background: #004400; border: 1px solid #0f0; color: #0f0; cursor: pointer; font-family: monospace; }
        .search-btn:hover { background: #006600; }
        .group { padding: 10px; margin: 5px 0; background: #1a1a1a; border-radius: 4px; display: flex; justify-content: space-between; align-items: center; }
        .group:hover { background: #2a2a2a; }
        .group-name { color: #0af; font-weight: bold; }
        .group-stats { color: #666; font-size: 0.9em; }
        a { color: #0af; text-decoration: none; }
        a:hover { color: #0f0; }
        .btn { display: inline-block; background: #004400; color: #0f0; padding: 6px 12px; border-radius: 3px; font-size: 0.9em; }
        .btn:hover { background: #006600; }
        .info { color: #888; margin: 15px 0; }
        .examples { background: #0a0a0a; padding: 10px; border-radius: 4px; margin-top: 10px; }
        .examples code { color: #f80; }
    </style>
</head>
<body>
    <div class="container">
        <h1>🔍 Search Newsgroups</h1>
        <p><a href="/">← Home</a> | Total groups: <strong>{{.TotalGroups}}</strong></p>

        <div class="search-box">
            <form action="/search" method="get">
                <input type="text" name="q" value="{{.Query}}" class="search-input" placeholder="Search pattern (e.g., comp.*, *linux*, alt.binaries.?)" autofocus>
                <button type="submit" class="search-btn">🔍 Search</button>
            </form>
            <div class="examples">
                <strong>Examples:</strong>
                <code>comp.*</code> |
                <code>alt.binaries.*</code> |
                <code>*linux*</code> |
                <code>*privacy*</code> |
                <code>sci.physics.*</code>
            </div>
        </div>

        {{if .Query}}
        <p class="info">Found <strong>{{.ResultCount}}</strong> groups matching "<code>{{.Query}}</code>"{{if .Truncated}} (showing first 500){{end}}</p>
        {{end}}

        {{if .Results}}
        <div id="results">
        {{range .Results}}
        <div class="group">
            <div>
                <a href="/group/{{.Name}}" class="group-name">{{.Name}}</a>
                <span class="group-stats">{{.Low}}-{{.High}} ({{.Status}})</span>
            </div>
            <a href="{{$.M2UsenetURL}}?newsgroups={{.Name}}" target="_blank" class="btn">📝 Post</a>
        </div>
        {{end}}
        </div>
        {{else if .Query}}
        <p>No groups found. Try different patterns like <code>*keyword*</code></p>
        {{else}}
        <p class="info">Enter a search pattern above. Use <code>*</code> for wildcards.</p>
        {{end}}
    </div>
</body>
</html>`

const groupTemplate = `<!DOCTYPE html>
<html>
<head>
    <title>📰 {{.GroupName}}</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #0f0; padding: 20px; }
        .container { max-width: 900px; margin: 0 auto; }
        .article { padding: 10px 12px; margin: 4px 0; background: #1a1a1a; border-radius: 4px; border-left: 3px solid #333; }
        .article:hover { background: #2a2a2a; }
        .article.depth-0 { border-left-color: #0f0; background: #1a1a1a; }
        .article.depth-1 { border-left-color: #0af; margin-left: 20px; }
        .article.depth-2 { border-left-color: #f80; margin-left: 40px; }
        .article.depth-3 { border-left-color: #f0f; margin-left: 60px; }
        .article.depth-4 { border-left-color: #ff0; margin-left: 80px; }
        .article.depth-5 { border-left-color: #0ff; margin-left: 100px; }
        .article.is-reply { font-size: 0.95em; }
        .subject { color: #0af; font-weight: bold; margin-bottom: 4px; display: flex; align-items: center; gap: 8px; }
        .subject a { color: #0af; text-decoration: none; }
        .subject a:hover { color: #0f0; }
        .thread-icon { font-size: 0.8em; opacity: 0.7; }
        .meta { color: #666; font-size: 0.8em; display: flex; justify-content: space-between; align-items: center; }
        a { color: #0af; text-decoration: none; }
        a:hover { color: #0f0; }
        .btn { display: inline-block; background: #004400; color: #0f0; padding: 8px 16px; border-radius: 4px; margin: 5px; }
        .btn:hover { background: #006600; }
        .btn-reply { background: #000044; border: 1px solid #0af; padding: 3px 8px; font-size: 0.75em; }
        .error { background: #2a0a0a; border: 1px solid #a00; color: #f88; padding: 15px; border-radius: 8px; margin: 15px 0; }
        .header { margin-bottom: 20px; padding-bottom: 15px; border-bottom: 1px solid #333; }
        .thread-info { color: #888; font-size: 0.85em; margin-bottom: 10px; }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>📰 {{.GroupName}}</h1>
            <p><a href="/">← Home</a> | <a href="/search">🔍 Search</a></p>
            <a href="{{.M2UsenetURL}}?newsgroups={{.GroupName}}" target="_blank" class="btn">📝 New Post</a>
        </div>

        {{if .Error}}
        <div class="error">❌ {{.Error}}</div>
        {{end}}

        {{if .Articles}}
        <h3>Threaded View ({{len .Articles}} articles)</h3>
        <p class="thread-info">🧵 Posts are organized by thread. Replies are indented under their parent.</p>
        {{range .Articles}}
        <div class="article depth-{{.Depth}}{{if gt .Depth 0}} is-reply{{end}}">
            <div class="subject">
                {{if gt .Depth 0}}<span class="thread-icon">↳</span>{{end}}
                <a href="/article/{{$.GroupName}}/{{.Number}}">{{.Subject}}</a>
            </div>
            <div class="meta">
                <span>{{.From}} | #{{.Number}}</span>
                <a href="{{$.M2UsenetURL}}?newsgroups={{$.GroupName}}&subject={{urlquery .ReplySubject}}&references={{urlquery .ReplyRef}}" target="_blank" class="btn btn-reply">💬 Reply</a>
            </div>
        </div>
        {{end}}
        {{else if not .Error}}
        <p>No articles found in this group.</p>
        {{end}}
    </div>
</body>
</html>`

const articleTemplate = `<!DOCTYPE html>
<html>
<head>
    <title>📄 Article {{.ArticleNum}} - {{.GroupName}}</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #0f0; padding: 20px; }
        .container { max-width: 900px; margin: 0 auto; }
        .header { margin-bottom: 20px; padding-bottom: 15px; border-bottom: 1px solid #333; }
        .headers-box { background: #1a1a1a; padding: 15px; border-radius: 8px; margin-bottom: 20px; border: 1px solid #333; }
        .header-line { margin: 5px 0; }
        .header-name { color: #f80; }
        .header-value { color: #0af; word-break: break-all; }
        .body-box { background: #1a1a1a; padding: 20px; border-radius: 8px; border: 1px solid #333; }
        .body-content { white-space: pre-wrap; word-wrap: break-word; line-height: 1.5; }
        .quote { color: #888; }
        a { color: #0af; text-decoration: none; }
        a:hover { color: #0f0; }
        .btn { display: inline-block; background: #004400; color: #0f0; padding: 8px 16px; border-radius: 4px; margin: 5px; }
        .btn:hover { background: #006600; }
        .btn-reply { background: #000044; border: 1px solid #0af; }
        .error { background: #2a0a0a; border: 1px solid #a00; color: #f88; padding: 15px; border-radius: 8px; margin: 15px 0; }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>📄 Article #{{.ArticleNum}}</h1>
            <p>
                <a href="/">← Home</a> | 
                <a href="/group/{{.GroupName}}">← {{.GroupName}}</a> |
                <a href="/search">🔍 Search</a>
            </p>
            {{if .ReplyURL}}
            <a href="{{.ReplyURL}}" target="_blank" class="btn btn-reply">💬 Reply</a>
            {{end}}
            <a href="{{.M2UsenetURL}}?newsgroups={{.GroupName}}" target="_blank" class="btn">📝 New Post</a>
        </div>

        {{if .Error}}
        <div class="error">❌ {{.Error}}</div>
        {{else}}

        <div class="headers-box">
            <h3>Headers</h3>
            {{if .Headers}}
            {{range $key, $value := .Headers}}
            <div class="header-line">
                <span class="header-name">{{$key}}:</span> 
                <span class="header-value">{{$value}}</span>
            </div>
            {{end}}
            {{end}}
        </div>

        <div class="body-box">
            <h3>Message</h3>
            <div class="body-content">{{.Body}}</div>
        </div>

        {{end}}
    </div>
</body>
</html>`

const healthTemplate = `<!DOCTYPE html>
<html>
<head>
    <title>🔍 Health Check</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #0f0; padding: 20px; }
        .container { max-width: 600px; margin: 0 auto; }
        .box { background: #1a1a1a; padding: 20px; border-radius: 8px; margin: 15px 0; border: 1px solid #333; }
        .ok { color: #0f0; }
        .err { color: #f80; }
        a { color: #0af; }
        code { background: #0a0a0a; padding: 2px 6px; border-radius: 2px; }
    </style>
</head>
<body>
    <div class="container">
        <h1>🔍 Health Check</h1>
        <p><a href="/">← Home</a></p>

        <div class="box">
            <h3>Connectivity</h3>
            <p>Tor Proxy (127.0.0.1:9050): {{if .TorOK}}<span class="ok">✅ OK</span>{{else}}<span class="err">❌ Failed</span>{{end}}</p>
            <p>NNTP Status: <span class="{{if eq .NNTPStatus "connected"}}ok{{else}}err{{end}}">{{.NNTPStatus}}</span></p>
            {{if .NNTPServer}}<p>Connected to: <code>{{.NNTPServer}}</code></p>{{end}}
        </div>

        <div class="box">
            <h3>Cache</h3>
            <p>Groups cached: <strong>{{.GroupCount}}</strong></p>
            <p>Session: <code>{{.Session}}</code></p>
            <p>Timestamp: {{.Timestamp}}</p>
        </div>

        <div class="box">
            <h3>Manual Tests</h3>
            <p><code>curl --socks5 127.0.0.1:9050 http://duckduckgogg42xjoc72x3sjasowoarfbgcmvfimaftt6twagswzczad.onion</code></p>
        </div>
    </div>
</body>
</html>`

// ============= Main =============

func main() {
	log.Println("🧅 Onion Newsreader starting...")

	server := NewNewsServer()
	defer server.client.Close()

	http.HandleFunc("/", server.handleHome)
	http.HandleFunc("/download", server.handleDownload)
	http.HandleFunc("/download-status", server.handleDownloadStatus)
	http.HandleFunc("/search", server.handleSearch)
	http.HandleFunc("/group/", server.handleGroup)
	http.HandleFunc("/article/", server.handleArticle)
	http.HandleFunc("/health", server.handleHealth)

	log.Println("🚀 HTTP server on 127.0.0.1:8080")
	log.Printf("📡 Primary: %s", PrimaryNNTP)
	log.Printf("🔄 Fallback: %s", FallbackNNTP)
	log.Printf("✍️ Posting: %s", M2UsenetURL)

	if err := http.ListenAndServe("127.0.0.1:8080", nil); err != nil {
		log.Fatal(err)
	}
}
