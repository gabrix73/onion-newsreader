package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"html"
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

type NewsgroupInfo struct {
	Name         string
	High         int
	Low          int
	Status       string
	ArticleCount int
}

type ArticleOverview struct {
	Number      int
	Subject     string
	From        string
	Date        string
	MessageID   string
	References  string
	ByteCount   int
	LineCount   int
	ParsedDate  time.Time
}

type ThreadNode struct {
	Article  *ArticleOverview
	Children []*ThreadNode
	Level    int
	IsRoot   bool
}

type Thread struct {
	Root         *ThreadNode
	ArticleCount int
	LastDate     time.Time
	Subject      string
}

type NNTPClient struct {
	conn           net.Conn
	reader         *bufio.Reader
	mutex          sync.Mutex
	lastUsed       time.Time
	isConnected    bool
	isConnecting   bool
	connectionChan chan struct{}
	backoffCount   int
	maxBackoff     time.Duration
	connectedServer string
	isOnionService  bool
}

type NewsServer struct {
	client        *NNTPClient
	groups        map[string]*NewsgroupInfo
	mutex         sync.RWMutex
	session       string
	features      map[string]bool
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
	return &NNTPClient{
		connectionChan: make(chan struct{}, 1),
		maxBackoff:     5 * time.Minute,
	}
}

func NewNewsServer() *NewsServer {
	sessionBytes := make([]byte, 8)
	rand.Read(sessionBytes)
	
	return &NewsServer{
		client:  NewNNTPClient(),
		groups:  make(map[string]*NewsgroupInfo),
		session: fmt.Sprintf("%x", sessionBytes),
		features: map[string]bool{
			"MODE_READER":    true,
			"XOVER":         true,
			"OVER":          true,
			"HDR":           true,
			"LIST_ACTIVE":   true,
			"LIST_NEWSGROUPS": true,
		},
	}
}

func (c *NNTPClient) connectInternal() error {
	proxyAddr := "127.0.0.1:9050"
	
	targetConfigs := []struct {
		addr        string
		isOnion     bool
		description string
	}{
		{
			addr:        "peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion:119",
			isOnion:     true,
			description: "Primary NNTP Onion Service",
		},
		{
			addr:        "news.tcpreset.net:119",
			isOnion:     false,
			description: "TCPReset NNTP Server (via Tor)",
		},
	}
	
	var lastErr error
	for i, config := range targetConfigs {
		log.Printf("🔗 Attempt %d/%d: %s - %s", i+1, len(targetConfigs), config.description, config.addr)

		var conn net.Conn
		var err error

		if config.isOnion {
			conn, err = c.connectViaSocks5(proxyAddr, config.addr)
		} else {
			conn, err = c.connectViaSocks5(proxyAddr, config.addr)
		}

		if err != nil {
			lastErr = fmt.Errorf("%s failed: %v", config.description, err)
			log.Printf("❌ %s", lastErr)
			continue
		}

		log.Printf("✅ Successfully connected to %s", config.description)
		c.conn = conn
		c.reader = bufio.NewReader(c.conn)
		c.isConnected = true
		c.lastUsed = time.Now()
		c.connectedServer = config.description
		c.isOnionService = config.isOnion

		log.Printf("📥 Reading NNTP welcome message from %s...", config.description)
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		welcome, err := c.reader.ReadString('\n')
		c.conn.SetReadDeadline(time.Time{})
		
		if err != nil {
			c.isConnected = false
			lastErr = fmt.Errorf("failed to read welcome from %s: %v", config.description, err)
			log.Printf("❌ %s", lastErr)
			c.conn.Close()
			c.connectedServer = ""
			c.isOnionService = false
			continue
		}

		welcome = strings.TrimSpace(welcome)
		log.Printf("📥 Welcome received from %s: %s", config.description, welcome)

		log.Printf("📤 Sending MODE READER command to %s...", config.description)
		return c.modeReaderInternal()
	}
	
	return fmt.Errorf("failed to connect to any NNTP server, last error: %v", lastErr)
}

func (c *NNTPClient) connectViaSocks5(proxyAddr, targetAddr string) (net.Conn, error) {
	proxyConn, err := net.DialTimeout("tcp", proxyAddr, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Tor proxy: %v", err)
	}

	deadline := 60 * time.Second
	if strings.Contains(targetAddr, ".onion") {
		deadline = 180 * time.Second
	}
	
	proxyConn.SetDeadline(time.Now().Add(deadline))
	defer proxyConn.SetDeadline(time.Time{})

	log.Printf("🔄 Performing SOCKS5 handshake for %s...", targetAddr)
	if err := c.socks5Handshake(proxyConn, targetAddr); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("SOCKS5 handshake failed: %v", err)
	}

	log.Printf("✅ SOCKS5 connection established to %s", targetAddr)
	return proxyConn, nil
}

func (c *NNTPClient) socks5Handshake(conn net.Conn, target string) error {
	log.Printf("🔄 SOCKS5: Starting authentication...")
	authReq := []byte{0x05, 0x01, 0x00}
	if _, err := conn.Write(authReq); err != nil {
		return fmt.Errorf("auth request failed: %v", err)
	}

	authResp := make([]byte, 2)
	if _, err := io.ReadFull(conn, authResp); err != nil {
		return fmt.Errorf("auth response read failed: %v", err)
	}

	if authResp[0] != 0x05 || authResp[1] != 0x00 {
		return fmt.Errorf("SOCKS5 authentication failed")
	}

	log.Printf("✅ SOCKS5: Authentication successful")

	parts := strings.Split(target, ":")
	host := parts[0]
	port, _ := strconv.Atoi(parts[1])

	log.Printf("🔄 SOCKS5: Connecting to %s (hostname length: %d)", target, len(host))
	req := []byte{0x05, 0x01, 0x00, 0x03}
	req = append(req, byte(len(host)))
	req = append(req, []byte(host)...)
	req = append(req, byte(port>>8), byte(port&0xff))

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("connect request failed: %v", err)
	}

	log.Printf("🔄 SOCKS5: Waiting for connect response...")
	resp := make([]byte, 4)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("connect response read failed: %v", err)
	}

	if resp[0] != 0x05 {
		return fmt.Errorf("SOCKS5 version mismatch")
	}
	
	if resp[1] != 0x00 {
		errorMsg := map[byte]string{
			0x01: "general SOCKS server failure",
			0x02: "connection not allowed by ruleset", 
			0x03: "network unreachable",
			0x04: "host unreachable",
			0x05: "connection refused",
			0x06: "TTL expired",
			0x07: "command not supported",
			0x08: "address type not supported",
		}
		if msg, ok := errorMsg[resp[1]]; ok {
			return fmt.Errorf("SOCKS5 connect failed: %s (code %d)", msg, resp[1])
		}
		return fmt.Errorf("SOCKS5 connect failed: error code %d", resp[1])
	}

	switch resp[3] {
	case 0x01:
		addr := make([]byte, 6)
		io.ReadFull(conn, addr)
	case 0x03:
		domainLen := make([]byte, 1)
		io.ReadFull(conn, domainLen)
		domain := make([]byte, domainLen[0]+2)
		io.ReadFull(conn, domain)
	case 0x04:
		addr := make([]byte, 18)
		io.ReadFull(conn, addr)
	}

	return nil
}

func (c *NNTPClient) ensureConnected() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	
	if !c.isConnected || c.conn == nil || time.Since(c.lastUsed) > 5*time.Minute {
		log.Printf("🔄 Connection check: reconnecting (connected=%v, lastUsed=%v ago)", 
			c.isConnected, time.Since(c.lastUsed))
		
		if c.conn != nil {
			c.conn.Close()
		}
		c.isConnected = false
		c.connectedServer = ""
		c.isOnionService = false
		
		if err := c.testOnionReachability(); err != nil {
			return fmt.Errorf("onion service unreachable: %v", err)
		}
		
		if err := c.connectInternal(); err != nil {
			return fmt.Errorf("failed to reconnect: %v", err)
		}
	}
	
	c.lastUsed = time.Now()
	return nil
}

func (c *NNTPClient) testOnionReachability() error {
	log.Printf("🧪 Testing NNTP server reachability...")
	
	proxyAddr := "127.0.0.1:9050"
	testConfigs := []struct {
		addr        string
		isOnion     bool
		description string
	}{
		{
			addr:        "peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion:119",
			isOnion:     true,
			description: "Primary NNTP Onion Service",
		},
		{
			addr:        "news.tcpreset.net:119",
			isOnion:     false,
			description: "TCPReset NNTP Server",
		},
	}
	
	for _, config := range testConfigs {
		log.Printf("🧪 Testing %s (%s)...", config.description, config.addr)
		
		proxyConn, err := net.DialTimeout("tcp", proxyAddr, 10*time.Second)
		if err != nil {
			log.Printf("⚠️ Tor proxy unreachable: %v", err)
			continue
		}
		defer proxyConn.Close()
		
		deadline := 30 * time.Second
		if config.isOnion {
			deadline = 60 * time.Second
		}
		proxyConn.SetDeadline(time.Now().Add(deadline))
		
		if err := c.quickSocks5Test(proxyConn, config.addr); err != nil {
			log.Printf("⚠️ %s failed: %v", config.description, err)
			continue
		}
		
		log.Printf("✅ %s appears reachable", config.description)
		return nil
	}
	
	return fmt.Errorf("no NNTP servers are reachable (tried onion service and public servers)")
}

func (c *NNTPClient) quickSocks5Test(conn net.Conn, target string) error {
	authReq := []byte{0x05, 0x01, 0x00}
	if _, err := conn.Write(authReq); err != nil {
		return fmt.Errorf("auth request failed: %v", err)
	}

	authResp := make([]byte, 2)
	if _, err := io.ReadFull(conn, authResp); err != nil {
		return fmt.Errorf("auth response read failed: %v", err)
	}

	if authResp[0] != 0x05 || authResp[1] != 0x00 {
		return fmt.Errorf("SOCKS5 authentication failed")
	}

	parts := strings.Split(target, ":")
	host := parts[0]
	port, _ := strconv.Atoi(parts[1])

	req := []byte{0x05, 0x01, 0x00, 0x03}
	req = append(req, byte(len(host)))
	req = append(req, []byte(host)...)
	req = append(req, byte(port>>8), byte(port&0xff))

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("connect request failed: %v", err)
	}

	resp := make([]byte, 4)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("connect response read failed: %v", err)
	}

	if resp[0] != 0x05 {
		return fmt.Errorf("SOCKS5 version mismatch")
	}
	
	if resp[1] != 0x00 {
		errorMsg := map[byte]string{
			0x01: "general SOCKS server failure",
			0x02: "connection not allowed by ruleset", 
			0x03: "network unreachable",
			0x04: "host unreachable",
			0x05: "connection refused",
			0x06: "TTL expired",
			0x07: "command not supported",
			0x08: "address type not supported",
		}
		if msg, ok := errorMsg[resp[1]]; ok {
			return fmt.Errorf("SOCKS5 connect failed: %s", msg)
		}
		return fmt.Errorf("SOCKS5 connect failed: error code %d", resp[1])
	}

	switch resp[3] {
	case 0x01:
		addr := make([]byte, 6)
		io.ReadFull(conn, addr)
	case 0x03:
		domainLen := make([]byte, 1)
		io.ReadFull(conn, domainLen)
		domain := make([]byte, domainLen[0]+2)
		io.ReadFull(conn, domain)
	case 0x04:
		addr := make([]byte, 18)
		io.ReadFull(conn, addr)
	}

	return nil
}

func (c *NNTPClient) modeReaderInternal() error {
	if err := c.sendCommand("MODE READER"); err != nil {
		return fmt.Errorf("failed to send MODE READER: %v", err)
	}

	response, err := c.readResponse()
	if err != nil {
		return fmt.Errorf("failed to read MODE READER response: %v", err)
	}

	if !strings.HasPrefix(response, "200") && !strings.HasPrefix(response, "201") {
		return fmt.Errorf("MODE READER failed: %s", response)
	}

	log.Printf("✅ MODE READER successful: %s", response)
	return nil
}

func (c *NNTPClient) sendCommand(command string) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	log.Printf("📤 Sending: %s", command)
	_, err := c.conn.Write([]byte(command + "\r\n"))
	return err
}

func (c *NNTPClient) readResponse() (string, error) {
	if c.reader == nil {
		return "", fmt.Errorf("not connected")
	}

	response, err := c.reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	response = strings.TrimSpace(response)
	log.Printf("📥 Received: %s", response)
	return response, nil
}

func (c *NNTPClient) HealthCheck() error {
	log.Printf("🧪 Performing NNTP health check...")
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if !c.isConnected || c.conn == nil {
		return fmt.Errorf("connection is nil or not connected")
	}

	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer c.conn.SetWriteDeadline(time.Time{})
	defer c.conn.SetReadDeadline(time.Time{})

	// Try DATE command first (single line response, safer)
	log.Printf("📤 Health check: sending DATE command")
	if err := c.sendCommand("DATE"); err != nil {
		c.isConnected = false
		return fmt.Errorf("health check send failed: %v", err)
	}

	response, err := c.readResponse()
	if err != nil {
		c.isConnected = false
		return fmt.Errorf("health check read failed: %v", err)
	}

	log.Printf("📥 Health check: received: %s", response)

	// DATE should return "111 YYYYMMDDhhmmss" - single line response
	if strings.HasPrefix(response, "111") {
		log.Printf("✅ Health check successful")
		return nil
	}

	// If DATE failed, try HELP but consume multiline response properly
	log.Printf("📤 Health check: DATE failed, trying HELP command")
	if err := c.sendCommand("HELP"); err != nil {
		c.isConnected = false
		return fmt.Errorf("health check HELP send failed: %v", err)
	}

	helpResponse, err := c.readResponse()
	if err != nil {
		c.isConnected = false
		return fmt.Errorf("health check HELP read failed: %v", err)
	}

	log.Printf("📥 Health check: HELP received: %s", helpResponse)

	// HELP command returns multiline response - consume all lines until "."
	if strings.HasPrefix(helpResponse, "100") {
		log.Printf("📥 Health check: reading multiline HELP response")
		for {
			line, err := c.reader.ReadString('\n')
			if err != nil {
				c.isConnected = false
				return fmt.Errorf("health check multiline read failed: %v", err)
			}
			
			line = strings.TrimSpace(line)
			log.Printf("📥 Health check help: %s", line)
			
			if line == "." {
				break
			}
		}
		log.Printf("✅ Health check successful")
		return nil
	}

	c.isConnected = false
	return fmt.Errorf("health check failed: unexpected response: %s", helpResponse)
}

func (c *NNTPClient) ListGroups() (map[string]*NewsgroupInfo, error) {
	log.Printf("Downloading active file with retry logic...")
	
	if err := c.ensureConnected(); err != nil {
		return nil, err
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	log.Printf("📤 Requesting group list...")
	if err := c.sendCommand("LIST"); err != nil {
		return nil, fmt.Errorf("failed to send LIST command: %v", err)
	}

	response, err := c.readResponse()
	if err != nil {
		return nil, fmt.Errorf("failed to read LIST response: %v", err)
	}

	if !strings.HasPrefix(response, "215") {
		return nil, fmt.Errorf("LIST command failed: %s", response)
	}

	log.Printf("📥 Reading group data...")
	groups := make(map[string]*NewsgroupInfo)
	lineCount := 0

	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("error reading group data: %v", err)
		}

		line = strings.TrimSpace(line)
		if line == "." {
			break
		}

		lineCount++
		if lineCount%1000 == 0 {
			log.Printf("📊 Processed %d groups...", lineCount)
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

	log.Printf("✅ Successfully loaded %d newsgroups", len(groups))
	return groups, nil
}

func (c *NNTPClient) SelectGroup(groupName string) (int, int, int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if !c.isConnected || c.conn == nil {
		return 0, 0, 0, fmt.Errorf("not connected")
	}

	log.Printf("📤 Selecting group: %s", groupName)
	if err := c.sendCommand("GROUP " + groupName); err != nil {
		return 0, 0, 0, fmt.Errorf("failed to send GROUP command: %v", err)
	}

	response, err := c.readResponse()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to read GROUP response: %v", err)
	}

	if !strings.HasPrefix(response, "211") {
		return 0, 0, 0, fmt.Errorf("GROUP command failed: %s", response)
	}

	// Parse response: 211 count low high group
	parts := strings.Fields(response)
	if len(parts) < 5 {
		return 0, 0, 0, fmt.Errorf("invalid GROUP response format: %s", response)
	}

	count, _ := strconv.Atoi(parts[1])
	low, _ := strconv.Atoi(parts[2])
	high, _ := strconv.Atoi(parts[3])

	log.Printf("✅ Group selected: %s (count=%d, range=%d-%d)", groupName, count, low, high)
	return count, low, high, nil
}

func (c *NNTPClient) GetArticleOverview(low, high int, maxArticles int) ([]*ArticleOverview, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if !c.isConnected || c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	// Limit range to prevent huge downloads
	if high-low+1 > maxArticles {
		low = high - maxArticles + 1
	}

	log.Printf("📤 Getting article overview for range %d-%d", low, high)
	if err := c.sendCommand(fmt.Sprintf("XOVER %d-%d", low, high)); err != nil {
		return nil, fmt.Errorf("failed to send XOVER command: %v", err)
	}

	response, err := c.readResponse()
	if err != nil {
		return nil, fmt.Errorf("failed to read XOVER response: %v", err)
	}

	if !strings.HasPrefix(response, "224") {
		return nil, fmt.Errorf("XOVER command failed: %s", response)
	}

	var articles []*ArticleOverview
	log.Printf("📥 Reading article overview data...")

	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("error reading overview data: %v", err)
		}

		line = strings.TrimSpace(line)
		if line == "." {
			break
		}

		article := c.parseOverviewLine(line)
		if article != nil {
			articles = append(articles, article)
		}
	}

	log.Printf("✅ Retrieved %d article overviews", len(articles))
	return articles, nil
}

func (c *NNTPClient) parseOverviewLine(line string) *ArticleOverview {
	// XOVER format: number<tab>subject<tab>from<tab>date<tab>message-id<tab>references<tab>byte count<tab>line count
	parts := strings.Split(line, "\t")
	if len(parts) < 8 {
		return nil
	}

	number, _ := strconv.Atoi(parts[0])
	byteCount, _ := strconv.Atoi(parts[6])
	lineCount, _ := strconv.Atoi(parts[7])

	// Parse the date for threading
	parsedDate := time.Now() // fallback
	if dateStr := strings.TrimSpace(parts[3]); dateStr != "" {
		// Try common date formats
		formats := []string{
			time.RFC1123Z,
			time.RFC1123,
			"Mon, 2 Jan 2006 15:04:05 -0700",
			"2 Jan 2006 15:04:05 -0700",
			"Mon, 2 Jan 2006 15:04:05 MST",
		}
		
		for _, format := range formats {
			if parsed, err := time.Parse(format, dateStr); err == nil {
				parsedDate = parsed
				break
			}
		}
	}

	return &ArticleOverview{
		Number:     number,
		Subject:    parts[1],
		From:       parts[2],
		Date:       parts[3],
		MessageID:  strings.TrimSpace(parts[4]),
		References: strings.TrimSpace(parts[5]),
		ByteCount:  byteCount,
		LineCount:  lineCount,
		ParsedDate: parsedDate,
	}
}

func (c *NNTPClient) GetArticle(groupName string, articleNumber int) (map[string]string, []string, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if !c.isConnected || c.conn == nil {
		return nil, nil, fmt.Errorf("not connected")
	}

	log.Printf("📤 Getting article %d from group %s", articleNumber, groupName)
	if err := c.sendCommand(fmt.Sprintf("ARTICLE %d", articleNumber)); err != nil {
		return nil, nil, fmt.Errorf("failed to send ARTICLE command: %v", err)
	}

	response, err := c.readResponse()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read ARTICLE response: %v", err)
	}

	if !strings.HasPrefix(response, "220") {
		return nil, nil, fmt.Errorf("ARTICLE command failed: %s", response)
	}

	// Read article content
	headers := make(map[string]string)
	var body []string
	inHeaders := true
	
	log.Printf("📥 Reading article content...")
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, nil, fmt.Errorf("error reading article data: %v", err)
		}

		line = strings.TrimSuffix(line, "\r\n")
		line = strings.TrimSuffix(line, "\n")
		
		if line == "." {
			break
		}

		if inHeaders {
			if line == "" {
				inHeaders = false
				continue
			}
			
			// Parse header line
			if strings.Contains(line, ":") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					key := strings.TrimSpace(parts[0])
					value := strings.TrimSpace(parts[1])
					
					// Handle multi-line headers
					if existing, exists := headers[key]; exists {
						headers[key] = existing + " " + value
					} else {
						headers[key] = value
					}
				}
			}
		} else {
			body = append(body, line)
		}
	}

	log.Printf("✅ Retrieved article %d: %d headers, %d body lines", articleNumber, len(headers), len(body))
	return headers, body, nil
}

func (c *NNTPClient) GetConnectionInfo() (string, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.connectedServer, c.isOnionService
}

func (c *NNTPClient) Close() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.conn != nil {
		log.Printf("📤 Sending: QUIT")
		c.sendCommand("QUIT")
		c.conn.Close()
		c.conn = nil
		c.reader = nil
	}
	c.isConnected = false
	c.connectedServer = ""
	c.isOnionService = false
	return nil
}

func (c *NNTPClient) disconnect() {
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn = nil
	c.reader = nil
	c.isConnected = false
	c.connectedServer = ""
	c.isOnionService = false
}

// Threading functions
func buildThreads(articles []*ArticleOverview) []*Thread {
	log.Printf("🧵 Building threads from %d articles...", len(articles))
	
	// Maps for fast lookup
	articlesByMessageID := make(map[string]*ArticleOverview)
	articlesBySubject := make(map[string][]*ArticleOverview)
	
	// Index articles
	for _, article := range articles {
		if article.MessageID != "" {
			articlesByMessageID[article.MessageID] = article
		}
		
		// Normalize subject for threading
		normalizedSubject := normalizeSubject(article.Subject)
		articlesBySubject[normalizedSubject] = append(articlesBySubject[normalizedSubject], article)
	}
	
	// Build thread trees
	threadRoots := make(map[string]*ThreadNode)
	processedArticles := make(map[string]bool)
	
	for _, article := range articles {
		if processedArticles[article.MessageID] {
			continue
		}
		
		// Find root of this thread
		root := findOrCreateThreadRoot(article, articlesByMessageID, threadRoots)
		buildThreadTree(root, article, articlesByMessageID, processedArticles)
	}
	
	// Convert to Thread structs and sort
	var threads []*Thread
	for _, root := range threadRoots {
		thread := &Thread{
			Root:         root,
			ArticleCount: countArticlesInThread(root),
			LastDate:     findLatestDateInThread(root),
			Subject:      root.Article.Subject,
		}
		threads = append(threads, thread)
	}
	
	// Sort threads by last activity
	sort.Slice(threads, func(i, j int) bool {
		return threads[i].LastDate.After(threads[j].LastDate)
	})
	
	log.Printf("✅ Built %d threads from %d articles", len(threads), len(articles))
	return threads
}

func normalizeSubject(subject string) string {
	// Remove Re:, Fwd:, etc.
	subject = strings.TrimSpace(subject)
	
	// Common prefixes to remove
	prefixes := []string{
		"Re:", "RE:", "re:", "Fwd:", "FWD:", "fwd:", 
		"Fw:", "FW:", "fw:", "AW:", "aw:", "Antw:",
	}
	
	for {
		trimmed := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(subject, prefix) {
				subject = strings.TrimSpace(subject[len(prefix):])
				trimmed = true
				break
			}
		}
		if !trimmed {
			break
		}
	}
	
	return strings.ToLower(subject)
}

func findOrCreateThreadRoot(article *ArticleOverview, articlesByMessageID map[string]*ArticleOverview, threadRoots map[string]*ThreadNode) *ThreadNode {
	// If no references, this is a root
	if article.References == "" {
		normalizedSubject := normalizeSubject(article.Subject)
		if root, exists := threadRoots[normalizedSubject]; exists {
			return root
		}
		
		root := &ThreadNode{
			Article: article,
			Level:   0,
			IsRoot:  true,
		}
		threadRoots[normalizedSubject] = root
		return root
	}
	
	// Find the root by following references
	references := parseReferences(article.References)
	if len(references) == 0 {
		normalizedSubject := normalizeSubject(article.Subject)
		if root, exists := threadRoots[normalizedSubject]; exists {
			return root
		}
		
		root := &ThreadNode{
			Article: article,
			Level:   0,
			IsRoot:  true,
		}
		threadRoots[normalizedSubject] = root
		return root
	}
	
	// The first reference is usually the thread root
	rootMessageID := references[0]
	if rootArticle, exists := articlesByMessageID[rootMessageID]; exists {
		normalizedSubject := normalizeSubject(rootArticle.Subject)
		if root, exists := threadRoots[normalizedSubject]; exists {
			return root
		}
		
		root := &ThreadNode{
			Article: rootArticle,
			Level:   0,
			IsRoot:  true,
		}
		threadRoots[normalizedSubject] = root
		return root
	}
	
	// Fallback: use subject-based threading
	normalizedSubject := normalizeSubject(article.Subject)
	if root, exists := threadRoots[normalizedSubject]; exists {
		return root
	}
	
	root := &ThreadNode{
		Article: article,
		Level:   0,
		IsRoot:  true,
	}
	threadRoots[normalizedSubject] = root
	return root
}

func buildThreadTree(root *ThreadNode, article *ArticleOverview, articlesByMessageID map[string]*ArticleOverview, processedArticles map[string]bool) {
	if processedArticles[article.MessageID] {
		return
	}
	
	processedArticles[article.MessageID] = true
	
	// If this is the root article, we're done
	if root.Article.MessageID == article.MessageID {
		return
	}
	
	// Find where to insert this article in the tree
	insertLocation := findInsertLocation(root, article, articlesByMessageID)
	
	newNode := &ThreadNode{
		Article: article,
		Level:   insertLocation.Level + 1,
		IsRoot:  false,
	}
	
	insertLocation.Children = append(insertLocation.Children, newNode)
	
	// Sort children by date
	sort.Slice(insertLocation.Children, func(i, j int) bool {
		return insertLocation.Children[i].Article.ParsedDate.Before(insertLocation.Children[j].Article.ParsedDate)
	})
}

func findInsertLocation(root *ThreadNode, article *ArticleOverview, articlesByMessageID map[string]*ArticleOverview) *ThreadNode {
	references := parseReferences(article.References)
	if len(references) == 0 {
		return root
	}
	
	// Find the parent by looking for the last reference that exists in our tree
	var parent *ThreadNode = root
	
	for i := len(references) - 1; i >= 0; i-- {
		parentMessageID := references[i]
		if foundParent := findNodeByMessageID(root, parentMessageID); foundParent != nil {
			parent = foundParent
			break
		}
	}
	
	return parent
}

func findNodeByMessageID(node *ThreadNode, messageID string) *ThreadNode {
	if node.Article.MessageID == messageID {
		return node
	}
	
	for _, child := range node.Children {
		if found := findNodeByMessageID(child, messageID); found != nil {
			return found
		}
	}
	
	return nil
}

func parseReferences(references string) []string {
	if references == "" {
		return nil
	}
	
	// Split by whitespace and clean up
	var messageIDs []string
	parts := strings.Fields(references)
	
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "<") && strings.HasSuffix(part, ">") {
			messageIDs = append(messageIDs, part)
		}
	}
	
	return messageIDs
}

func countArticlesInThread(node *ThreadNode) int {
	count := 1
	for _, child := range node.Children {
		count += countArticlesInThread(child)
	}
	return count
}

func findLatestDateInThread(node *ThreadNode) time.Time {
	latest := node.Article.ParsedDate
	
	for _, child := range node.Children {
		childLatest := findLatestDateInThread(child)
		if childLatest.After(latest) {
			latest = childLatest
		}
	}
	
	return latest
}

func flattenThread(node *ThreadNode) []*ArticleOverview {
	var articles []*ArticleOverview
	articles = append(articles, node.Article)
	
	for _, child := range node.Children {
		articles = append(articles, flattenThread(child)...)
	}
	
	return articles
}

func (s *NewsServer) GetHealthStatus() map[string]interface{} {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	status := map[string]interface{}{
		"session":       s.session,
		"timestamp":     time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		"features":      s.features,
		"target":        "peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion",
		"fallback":      "news.tcpreset.net",
		"cached_groups": len(s.groups),
	}

	if connectedServer, isOnion := s.client.GetConnectionInfo(); connectedServer != "" {
		status["connected_server"] = connectedServer
		status["is_onion_service"] = isOnion
	}

	if err := s.testTorProxy(); err != nil {
		status["overall"] = "degraded"
		status["tor_proxy"] = "failed"
		status["tor_error"] = err.Error()
		status["nntp_status"] = "untested"
		status["message"] = "Tor proxy is not reachable - check if Tor is running"
		return status
	}
	
	status["tor_proxy"] = "ok"

	if err := s.client.HealthCheck(); err != nil {
		status["overall"] = "partial"
		status["nntp_status"] = "disconnected"
		status["nntp_error"] = err.Error()
		status["http_status"] = "ok"
		
		if strings.Contains(err.Error(), "TTL expired") {
			status["message"] = "Onion service appears to be down or unreachable"
		} else if strings.Contains(err.Error(), "connection reset") {
			status["message"] = "Connection to onion service was reset"
		} else if strings.Contains(err.Error(), "timeout") {
			status["message"] = "Connection to onion service timed out"
		} else {
			status["message"] = "HTTP server running, NNTP connection will retry automatically"
		}
	} else {
		status["overall"] = "healthy"
		status["nntp_status"] = "connected"
		status["http_status"] = "ok"
		status["message"] = "All systems operational"
	}

	return status
}

func (s *NewsServer) testTorProxy() error {
	log.Printf("🧪 Testing Tor proxy connectivity...")
	
	conn, err := net.DialTimeout("tcp", "127.0.0.1:9050", 10*time.Second)
	if err != nil {
		return fmt.Errorf("Tor proxy not reachable: %v", err)
	}
	defer conn.Close()
	
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	
	_, err = conn.Write([]byte{0x05, 0x01, 0x00})
	if err != nil {
		return fmt.Errorf("SOCKS5 handshake failed: %v", err)
	}
	
	resp := make([]byte, 2)
	_, err = io.ReadFull(conn, resp)
	if err != nil {
		return fmt.Errorf("SOCKS5 response failed: %v", err)
	}
	
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("SOCKS5 authentication failed: got %d,%d", resp[0], resp[1])
	}
	
	log.Printf("✅ Tor proxy is reachable and responding")
	return nil
}

func (s *NewsServer) downloadActiveFile() error {
	s.downloadStatus.mutex.Lock()
	s.downloadStatus.IsDownloading = true
	s.downloadStatus.StartTime = time.Now()
	s.downloadStatus.LastError = ""
	s.downloadStatus.Completed = false
	s.downloadStatus.mutex.Unlock()
	
	log.Printf("📥 Starting active file download with timeout...")
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	
	resultChan := make(chan error, 1)
	
	go func() {
		groups, err := s.client.ListGroups()
		if err != nil {
			resultChan <- err
			return
		}

		s.mutex.Lock()
		s.groups = groups
		s.mutex.Unlock()
		
		s.downloadStatus.mutex.Lock()
		s.downloadStatus.GroupCount = len(groups)
		s.downloadStatus.mutex.Unlock()
		
		resultChan <- nil
	}()
	
	select {
	case err := <-resultChan:
		s.downloadStatus.mutex.Lock()
		s.downloadStatus.IsDownloading = false
		s.downloadStatus.mutex.Unlock()
		
		if err != nil {
			log.Printf("❌ Active file download failed: %v", err)
			s.downloadStatus.mutex.Lock()
			s.downloadStatus.LastError = err.Error()
			s.downloadStatus.mutex.Unlock()
			return err
		}
		log.Printf("✅ Active file download completed successfully")
		s.downloadStatus.mutex.Lock()
		s.downloadStatus.Completed = true
		s.downloadStatus.mutex.Unlock()
		return nil
	case <-ctx.Done():
		log.Printf("⏰ Active file download timed out after 5 minutes")
		s.downloadStatus.mutex.Lock()
		s.downloadStatus.IsDownloading = false
		s.downloadStatus.LastError = "Download timed out after 5 minutes"
		s.downloadStatus.mutex.Unlock()
		return fmt.Errorf("download timed out - onion service connection taking too long")
	}
}

func (s *NewsServer) handleDownloadStatus(w http.ResponseWriter, r *http.Request) {
	s.downloadStatus.mutex.RLock()
	isDownloading := s.downloadStatus.IsDownloading
	startTime := s.downloadStatus.StartTime
	lastError := s.downloadStatus.LastError
	completed := s.downloadStatus.Completed
	groupCount := s.downloadStatus.GroupCount
	s.downloadStatus.mutex.RUnlock()
	
	w.Header().Set("Content-Type", "application/json")
	
	status := map[string]interface{}{
		"is_downloading": isDownloading,
		"completed": completed,
		"group_count": groupCount,
		"timestamp": time.Now().Unix(),
	}
	
	if !startTime.IsZero() {
		status["start_time"] = startTime.Unix()
		status["elapsed_seconds"] = int(time.Since(startTime).Seconds())
	}
	
	if lastError != "" {
		status["last_error"] = lastError
	}
	
	if isDownloading {
		status["message"] = "Download in progress..."
	} else if completed {
		status["message"] = "Download completed successfully"
	} else if lastError != "" {
		status["message"] = "Download failed"
	} else {
		status["message"] = "No download in progress"
	}
	
	w.Write([]byte(fmt.Sprintf(`{
		"is_downloading": %t,
		"completed": %t,
		"group_count": %d,
		"message": "%s",
		"timestamp": %d
		%s
	}`, 
		isDownloading, 
		completed, 
		groupCount,
		status["message"],
		status["timestamp"],
		func() string {
			extras := ""
			if !startTime.IsZero() {
				extras += fmt.Sprintf(`,
		"elapsed_seconds": %d`, int(time.Since(startTime).Seconds()))
			}
			if lastError != "" {
				extras += fmt.Sprintf(`,
		"last_error": "%s"`, lastError)
			}
			return extras
		}(),
	)))
}

func (s *NewsServer) handleActive(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" || r.Method == "POST" {
		log.Printf("📥 Active file download requested...")
		
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		processingHTML := `<!DOCTYPE html>
<html>
<head>
    <title>⏳ Downloading... - Onion Newsreader</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #ffaa00; padding: 20px; }
        .container { max-width: 800px; margin: 0 auto; text-align: center; }
        .spinner { animation: spin 1s linear infinite; display: inline-block; font-size: 2em; }
        @keyframes spin { 0% { transform: rotate(0deg); } 100% { transform: rotate(360deg); } }
        .processing-box { background: #1a1a1a; padding: 30px; border-radius: 8px; border: 2px solid #ffaa00; margin: 20px 0; }
        .back-link { color: #00aaff; text-decoration: none; }
        .back-link:hover { color: #00ff00; }
        .status-update { margin: 15px 0; padding: 10px; background: #2a1a00; border-radius: 4px; }
        .success { color: #00ff00; }
        .error { color: #ff8800; }
        .progress { margin: 10px 0; font-size: 0.9em; color: #888; }
        .app-footer {
            margin-top: 40px;
            padding: 25px 0;
            border-top: 2px solid #333;
            background: #1a1a1a;
            text-align: center;
        }
        .footer-links {
            display: flex;
            justify-content: center;
            gap: 25px;
            flex-wrap: wrap;
        }
        .footer-links a {
            color: #00aaff;
            text-decoration: none;
            font-weight: 500;
            padding: 8px 15px;
            border-radius: 20px;
            background: rgba(0, 170, 255, 0.1);
            transition: all 0.3s ease;
            border: 1px solid rgba(0, 170, 255, 0.2);
        }
        .footer-links a:hover {
            background: #00aaff;
            color: #0a0a0a;
        }
        .footer-links a[href^="monero:"] {
            background: rgba(255, 102, 0, 0.1);
            color: #ff6600;
            border-color: rgba(255, 102, 0, 0.2);
        }
        .footer-links a[href^="monero:"]:hover {
            background: #ff6600;
            color: #0a0a0a;
        }
        .footer-links a[href*="github"] {
            background: rgba(51, 51, 51, 0.1);
            color: #00ff00;
            border-color: rgba(51, 51, 51, 0.2);
        }
        .footer-links a[href*="github"]:hover {
            background: #00ff00;
            color: #0a0a0a;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="processing-box">
            <h1><span class="spinner" id="spinner">🔄</span> <span id="status-title">Downloading Active File</span></h1>
            <p id="status-text"><strong>Connecting to NNTP server via Tor...</strong></p>
            <div class="status-update" id="status-details">
                <p>⏰ <strong>This may take 2-5 minutes</strong> due to onion service connection time</p>
                <p id="progress-text">🔍 Checking status...</p>
            </div>
            <p>
                <a href="/active" class="back-link">🔄 Check Progress</a> | 
                <a href="/" class="back-link">← Back to Home</a> | 
                <a href="/health" class="back-link">📊 Health Status</a>
            </p>
            <p class="progress"><em>Started at ` + time.Now().Format("15:04:05") + `</em></p>
        </div>
        
        <footer class="app-footer">
            <div class="footer-links">
                <a href="https://github.com/gabrix73/onion-newsreader.git" target="_blank" rel="noopener">📁 Source Code</a>
                <a href="monero:44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L" onclick="fallbackCopyXMR(event)">⚡ Support Project</a>
                <a href="https://www.virebent.art" target="_blank" rel="noopener">🌐 virebent.art</a>
            </div>
        </footer>
    </div>
    <script>
        let checkCount = 0;
        
        function updateStatus() {
            checkCount++;
            fetch('/download-status')
                .then(response => response.json())
                .then(data => {
                    const spinner = document.getElementById('spinner');
                    const statusTitle = document.getElementById('status-title');
                    const statusText = document.getElementById('status-text');
                    const progressText = document.getElementById('progress-text');
                    
                    if (data.completed) {
                        spinner.innerHTML = '✅';
                        spinner.className = 'success';
                        statusTitle.innerHTML = 'Download Completed!';
                        statusText.innerHTML = '<strong>Successfully downloaded ' + data.group_count + ' newsgroups</strong>';
                        progressText.innerHTML = '🎉 Redirecting to active groups page...';
                        setTimeout(() => { window.location.href = '/active'; }, 3000);
                    } else if (data.last_error) {
                        spinner.innerHTML = '❌';
                        spinner.className = 'error';
                        statusTitle.innerHTML = 'Download Failed';
                        statusText.innerHTML = '<strong>Error: ' + data.last_error + '</strong>';
                        progressText.innerHTML = '🔄 <a href="/active?refresh=1" style="color: #00aaff;">Try Again</a>';
                    } else if (data.is_downloading) {
                        statusText.innerHTML = '<strong>Download in progress...</strong>';
                        if (data.elapsed_seconds) {
                            progressText.innerHTML = '⏰ Running for ' + data.elapsed_seconds + ' seconds...';
                        }
                    } else {
                        progressText.innerHTML = '⏰ Waiting for download to start... (check ' + checkCount + ')';
                    }
                })
                .catch(error => {
                    console.log('Status check failed:', error);
                    document.getElementById('progress-text').innerHTML = '⚠️ Status check failed, will retry...';
                });
        }
        
        updateStatus();
        const interval = setInterval(updateStatus, 3000);
        
        setTimeout(() => {
            clearInterval(interval);
            document.getElementById('progress-text').innerHTML = '⏰ Status checking stopped - please refresh page manually';
        }, 10 * 60 * 1000);

        function fallbackCopyXMR(event) {
            const xmrAddress = "44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L";
            
            if (!navigator.userAgent.includes('Monero')) {
                event.preventDefault();
                if (navigator.clipboard && navigator.clipboard.writeText) {
                    navigator.clipboard.writeText(xmrAddress).then(() => {
                        alert("✅ XMR address copied to clipboard!");
                    }).catch(() => {
                        prompt("Copy XMR address:", xmrAddress);
                    });
                } else {
                    prompt("Copy XMR address:", xmrAddress);
                }
            }
        }
    </script>
</body>
</html>`
		w.Write([]byte(processingHTML))
		
		go func() {
			log.Printf("🔄 Starting background active file download...")
			err := s.downloadActiveFile()
			if err != nil {
				log.Printf("❌ Background download failed: %v", err)
			} else {
				log.Printf("✅ Background download completed successfully")
			}
		}()
		
		return
	}

	s.mutex.RLock()
	groupCount := len(s.groups)
	s.mutex.RUnlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	activeHTML := `<!DOCTYPE html>
<html>
<head>
    <title>📋 Active Groups - Onion Newsreader</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #00ff00; padding: 20px; }
        .container { max-width: 1000px; margin: 0 auto; }
        .header { margin-bottom: 20px; }
        .download-section { background: #1a1a1a; padding: 20px; border-radius: 8px; margin-bottom: 20px; }
        .download-button { background: #333; color: #00ff00; border: none; padding: 12px 20px; border-radius: 4px; cursor: pointer; }
        .download-button:hover { background: #555; }
        .group-list { background: #1a1a1a; padding: 20px; border-radius: 8px; }
        .group-item { padding: 8px; margin: 3px 0; background: #0a0a0a; border-radius: 3px; }
        .group-name { color: #00aaff; }
        .group-stats { color: #888; font-size: 0.9em; }
        .back-link { color: #00aaff; text-decoration: none; }
        .back-link:hover { color: #00ff00; }
        .warning { background: #2a1a00; border: 1px solid #ffaa00; color: #ffaa00; padding: 15px; border-radius: 8px; margin-bottom: 20px; }
        .success { background: #002a1a; border: 1px solid #00aa00; color: #00aa00; padding: 15px; border-radius: 8px; margin-bottom: 20px; }
        .app-footer {
            margin-top: 40px;
            padding: 25px 0;
            border-top: 2px solid #333;
            background: #1a1a1a;
            text-align: center;
        }
        .footer-links {
            display: flex;
            justify-content: center;
            gap: 25px;
            flex-wrap: wrap;
        }
        .footer-links a {
            color: #00aaff;
            text-decoration: none;
            font-weight: 500;
            padding: 8px 15px;
            border-radius: 20px;
            background: rgba(0, 170, 255, 0.1);
            transition: all 0.3s ease;
            border: 1px solid rgba(0, 170, 255, 0.2);
        }
        .footer-links a:hover {
            background: #00aaff;
            color: #0a0a0a;
        }
        .footer-links a[href^="monero:"] {
            background: rgba(255, 102, 0, 0.1);
            color: #ff6600;
            border-color: rgba(255, 102, 0, 0.2);
        }
        .footer-links a[href^="monero:"]:hover {
            background: #ff6600;
            color: #0a0a0a;
        }
        .footer-links a[href*="github"] {
            background: rgba(51, 51, 51, 0.1);
            color: #00ff00;
            border-color: rgba(51, 51, 51, 0.2);
        }
        .footer-links a[href*="github"]:hover {
            background: #00ff00;
            color: #0a0a0a;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>📋 Active Newsgroups</h1>
            <p><a href="/" class="back-link">← Back to Home</a> | <a href="/health" class="back-link">📊 Health Status</a></p>
        </div>

        {{if eq .GroupCount 0}}
        <div class="warning">
            <h3>⚠️ No Groups Cached</h3>
            <p>No newsgroups are currently cached. You need to download the active file first.</p>
            <p><em>Note: This may take 2-5 minutes due to onion service connection time.</em></p>
        </div>
        {{else}}
        <div class="success">
            <h3>✅ Groups Loaded</h3>
            <p>Currently cached: <strong>{{.GroupCount}}</strong> newsgroups</p>
            <p>You can now browse groups or download a fresh list.</p>
        </div>
        {{end}}

        <div class="download-section">
            <h3>📡 Download Active File</h3>
            <p>Current groups in cache: <strong>{{.GroupCount}}</strong></p>
            <form method="post" onsubmit="this.querySelector('button').innerHTML='⏳ Starting...'; this.querySelector('button').disabled=true;">
                <button type="submit" class="download-button">🔄 START DOWNLOAD</button>
            </form>
            <p><em>This will download the complete list of available newsgroups from the server.</em></p>
            <p><strong>⏰ Please be patient:</strong> Onion service connections can take 2-5 minutes. The page will show progress.</p>
        </div>

        {{if .HasGroups}}
        <div class="group-list">
            <h3>📊 Available Groups (showing first 100)</h3>
            {{range .Groups}}
            <div class="group-item">
                <div class="group-name">
                    <a href="/group/{{.Name}}" style="color: #00aaff; text-decoration: none;">{{.Name}}</a>
                </div>
                <div class="group-stats">Range: {{.Low}}-{{.High}} ({{.Status}})</div>
            </div>
            {{end}}
        </div>
        {{end}}
        
        <footer class="app-footer">
            <div class="footer-links">
                <a href="https://github.com/gabrix73/onion-newsreader.git" target="_blank" rel="noopener">📁 Source Code</a>
                <a href="monero:44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L" onclick="fallbackCopyXMR(event)">⚡ Support Project</a>
                <a href="https://www.virebent.art" target="_blank" rel="noopener">🌐 virebent.art</a>
            </div>
        </footer>
    </div>
    
    <script>
    function fallbackCopyXMR(event) {
        const xmrAddress = "44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L";
        
        if (!navigator.userAgent.includes('Monero')) {
            event.preventDefault();
            if (navigator.clipboard && navigator.clipboard.writeText) {
                navigator.clipboard.writeText(xmrAddress).then(() => {
                    alert("✅ XMR address copied to clipboard!");
                }).catch(() => {
                    prompt("Copy XMR address:", xmrAddress);
                });
            } else {
                prompt("Copy XMR address:", xmrAddress);
            }
        }
    }
    </script>
</body>
</html>`

	var displayGroups []*NewsgroupInfo
	if groupCount > 0 {
		s.mutex.RLock()
		for _, group := range s.groups {
			if len(displayGroups) >= 100 {
				break
			}
			displayGroups = append(displayGroups, group)
		}
		s.mutex.RUnlock()

		sort.Slice(displayGroups, func(i, j int) bool {
			return displayGroups[i].Name < displayGroups[j].Name
		})
	}

	tmpl, _ := template.New("active").Parse(activeHTML)
	data := map[string]interface{}{
		"GroupCount": groupCount,
		"Groups":     displayGroups,
		"HasGroups":  len(displayGroups) > 0,
	}
	tmpl.Execute(w, data)
}

func (s *NewsServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := s.GetHealthStatus()
	
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	
	healthHTML := `<!DOCTYPE html>
<html>
<head>
    <title>📊 System Health - Onion Newsreader</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #00ff00; padding: 40px; }
        .status-ok { color: #00ff00; }
        .status-warn { color: #ffaa00; }
        .status-error { color: #ff0000; }
        .status-degraded { color: #ffaa00; }
        .status-partial { color: #ffaa00; }
        .status-connected { color: #00ff00; }
        .status-disconnected { color: #ff8800; }
        .status-failed { color: #ff0000; }
        .container { max-width: 800px; margin: 0 auto; }
        .status-item { margin: 10px 0; padding: 10px; background: #1a1a1a; border-radius: 5px; }
        .diagnostic-section { background: #2a2a1a; padding: 15px; border-radius: 5px; margin: 20px 0; }
        .diagnostic-test { margin: 8px 0; padding: 8px; background: #1a1a1a; border-radius: 3px; font-size: 0.9em; }
        .back-link { color: #00aaff; text-decoration: none; }
        .back-link:hover { color: #00ff00; }
        .test-button { background: #333; color: #00ff00; border: none; padding: 8px 16px; border-radius: 4px; cursor: pointer; margin: 5px; }
        .test-button:hover { background: #555; }
        .app-footer {
            margin-top: 40px;
            padding: 25px 0;
            border-top: 2px solid #333;
            background: #1a1a1a;
            text-align: center;
        }
        .footer-links {
            display: flex;
            justify-content: center;
            gap: 25px;
            flex-wrap: wrap;
        }
        .footer-links a {
            color: #00aaff;
            text-decoration: none;
            font-weight: 500;
            padding: 8px 15px;
            border-radius: 20px;
            background: rgba(0, 170, 255, 0.1);
            transition: all 0.3s ease;
            border: 1px solid rgba(0, 170, 255, 0.2);
        }
        .footer-links a:hover {
            background: #00aaff;
            color: #0a0a0a;
        }
        .footer-links a[href^="monero:"] {
            background: rgba(255, 102, 0, 0.1);
            color: #ff6600;
            border-color: rgba(255, 102, 0, 0.2);
        }
        .footer-links a[href^="monero:"]:hover {
            background: #ff6600;
            color: #0a0a0a;
        }
        .footer-links a[href*="github"] {
            background: rgba(51, 51, 51, 0.1);
            color: #00ff00;
            border-color: rgba(51, 51, 51, 0.2);
        }
        .footer-links a[href*="github"]:hover {
            background: #00ff00;
            color: #0a0a0a;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>📊 System Health Status</h1>
        
        <div class="status-item">
            <strong>Overall Status:</strong> 
            <span class="status-{{.OverallClass}}">{{.Overall}}</span>
        </div>
        
        {{if .TorProxy}}
        <div class="status-item">
            <strong>Tor Proxy:</strong> 
            <span class="status-{{.TorProxyClass}}">{{.TorProxy}}</span>
            {{if .TorError}}
            <br><small style="color: #ff8800;">{{.TorError}}</small>
            {{end}}
        </div>
        {{end}}
        
        <div class="status-item">
            <strong>NNTP Connection:</strong> 
            <span class="status-{{.NNTPClass}}">{{.NNTPStatus}}</span>
            {{if .NNTPError}}
            <br><small style="color: #ff8800;">{{.NNTPError}}</small>
            {{end}}
        </div>
        
        <div class="status-item">
            <strong>Target Server:</strong> {{.Target}}
            <br><strong>Fallback Server:</strong> {{.Fallback}} (via Tor)
            {{if .ConnectedServer}}
            <br><strong>Currently Connected:</strong> {{.ConnectedServer}} 
            {{if .IsOnionService}}(🧅 Onion Service){{else}}(🌐 Via Tor){{end}}
            {{end}}
        </div>
        
        <div class="status-item">
            <strong>Cached Groups:</strong> {{.CachedGroups}}
        </div>
        
        <div class="status-item">
            <strong>Timestamp:</strong> {{.Timestamp}}
        </div>
        
        <div class="status-item">
            <strong>Session:</strong> {{.Session}}
        </div>
        
        {{if .Message}}
        <div class="status-item">
            <strong>Status:</strong> {{.Message}}
        </div>
        {{end}}
        
        <div class="diagnostic-section">
            <h3>🔧 Diagnostic Tools</h3>
            <p>Manual tests you can run to troubleshoot connectivity:</p>
            
            <div class="diagnostic-test">
                <strong>1. Test Tor connectivity (v3 onion):</strong><br>
                <code>curl --socks5 127.0.0.1:9050 http://duckduckgogg42xjoc72x3sjasowoarfbgcmvfimaftt6twagswzczad.onion</code>
            </div>
            
            <div class="diagnostic-test">
                <strong>2a. Test primary onion service:</strong><br>
                <code>timeout 120 curl --socks5 127.0.0.1:9050 -v telnet://peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion:119</code>
            </div>
            
            <div class="diagnostic-test">
                <strong>2b. Test fallback NNTP server:</strong><br>
                <code>timeout 60 curl --socks5 127.0.0.1:9050 -v telnet://news.tcpreset.net:119</code>
            </div>
            
            <div class="diagnostic-test">
                <strong>3. Check Tor service:</strong><br>
                <code>systemctl status tor</code> or <code>ps aux | grep tor</code>
            </div>
            
            <div class="diagnostic-test">
                <strong>4. Check Tor logs:</strong><br>
                <code>journalctl -u tor -f</code> or <code>tail -f /var/log/tor/tor.log</code>
            </div>
            
            <p style="margin-top: 15px;"><em>If Tor proxy test fails, Tor is not running or configured correctly.</em></p>
            <p><em>If onion service test fails, the target service may be down.</em></p>
            <p><em>Try the <a href="/test-onion" class="back-link">Interactive Test Page</a> for detailed diagnostics.</em></p>
        </div>
        
        <div class="status-item">
            <strong>Features:</strong>
            <ul>
                <li>✅ Auto-reconnect enabled</li>
                <li>✅ MODE READER support</li>
                <li>✅ Health monitoring</li>
                <li>✅ Connection retry logic</li>
                <li>✅ Broken pipe recovery</li>
                <li>✅ Diagnostic testing</li>
                <li>✅ Thread-based article grouping</li>
            </ul>
        </div>
        
        <p><a href="/" class="back-link">← Back to Home</a> | <a href="/active" class="back-link">Active Groups</a></p>
        
        <footer class="app-footer">
            <div class="footer-links">
                <a href="https://github.com/gabrix73/onion-newsreader.git" target="_blank" rel="noopener">📁 Source Code</a>
                <a href="monero:44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L" onclick="fallbackCopyXMR(event)">⚡ Support Project</a>
                <a href="https://www.virebent.art" target="_blank" rel="noopener">🌐 virebent.art</a>
            </div>
        </footer>
    </div>
    
    <script>
        setTimeout(function() {
            window.location.reload();
        }, 30000);

        function fallbackCopyXMR(event) {
            const xmrAddress = "44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L";
            
            if (!navigator.userAgent.includes('Monero') {
                event.preventDefault();
                if (navigator.clipboard && navigator.clipboard.writeText) {
                    navigator.clipboard.writeText(xmrAddress).then(() => {
                        alert("✅ XMR address copied to clipboard!");
                    }).catch(() => {
                        prompt("Copy XMR address:", xmrAddress);
                    });
                } else {
                    prompt("Copy XMR address:", xmrAddress);
                }
            }
        }
    </script>
</body>
</html>`

	tmpl, _ := template.New("health").Parse(healthHTML)
	
	data := map[string]interface{}{
		"Overall":         status["overall"],
		"OverallClass":    strings.ToLower(fmt.Sprintf("%v", status["overall"])),
		"NNTPStatus":      status["nntp_status"],
		"NNTPClass":       strings.ToLower(fmt.Sprintf("%v", status["nntp_status"])),
		"Target":          status["target"],
		"Fallback":        status["fallback"],
		"CachedGroups":    status["cached_groups"],
		"Timestamp":       status["timestamp"],
		"Session":         status["session"],
		"Message":         status["message"],
	}
	
	if connectedServer, exists := status["connected_server"]; exists {
		data["ConnectedServer"] = connectedServer
	}
	if isOnionService, exists := status["is_onion_service"]; exists {
		data["IsOnionService"] = isOnionService
	}
	
	if torProxy, exists := status["tor_proxy"]; exists {
		data["TorProxy"] = torProxy
		data["TorProxyClass"] = strings.ToLower(fmt.Sprintf("%v", torProxy))
	}
	
	if nntpError, exists := status["nntp_error"]; exists {
		data["NNTPError"] = nntpError
	}
	
	if torError, exists := status["tor_error"]; exists {
		data["TorError"] = torError
	}
	
	tmpl.Execute(w, data)
}

func (s *NewsServer) handleTestOnion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	
	testHTML := `<!DOCTYPE html>
<html>
<head>
    <title>🧪 Onion Service Test - Onion Newsreader</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #00ff00; padding: 20px; }
        .container { max-width: 800px; margin: 0 auto; }
        .test-box { background: #1a1a1a; padding: 20px; border-radius: 8px; margin: 20px 0; border: 1px solid #333; }
        .back-link { color: #00aaff; text-decoration: none; }
        .back-link:hover { color: #00ff00; }
        .test-button { background: #333; color: #00ff00; border: none; padding: 12px 20px; border-radius: 4px; cursor: pointer; margin: 5px; }
        .test-button:hover { background: #555; }
        .result { margin: 15px 0; padding: 15px; border-radius: 5px; }
        .result-success { background: #002a1a; border: 1px solid #00aa00; color: #00aa00; }
        .result-error { background: #2a1a1a; border: 1px solid #aa0000; color: #aa0000; }
        .result-info { background: #1a2a2a; border: 1px solid #0088aa; color: #0088aa; }
        .spinner { animation: spin 1s linear infinite; display: inline-block; }
        @keyframes spin { 0% { transform: rotate(0deg); } 100% { transform: rotate(360deg); } }
        .test-progress { display: none; }
        pre { background: #0a0a0a; padding: 10px; border-radius: 3px; overflow-x: auto; }
        .app-footer {
            margin-top: 40px;
            padding: 25px 0;
            border-top: 2px solid #333;
            background: #1a1a1a;
            text-align: center;
        }
        .footer-links {
            display: flex;
            justify-content: center;
            gap: 25px;
            flex-wrap: wrap;
        }
        .footer-links a {
            color: #00aaff;
            text-decoration: none;
            font-weight: 500;
            padding: 8px 15px;
            border-radius: 20px;
            background: rgba(0, 170, 255, 0.1);
            transition: all 0.3s ease;
            border: 1px solid rgba(0, 170, 255, 0.2);
        }
        .footer-links a:hover {
            background: #00aaff;
            color: #0a0a0a;
        }
        .footer-links a[href^="monero:"] {
            background: rgba(255, 102, 0, 0.1);
            color: #ff6600;
            border-color: rgba(255, 102, 0, 0.2);
        }
        .footer-links a[href^="monero:"]:hover {
            background: #ff6600;
            color: #0a0a0a;
        }
        .footer-links a[href*="github"] {
            background: rgba(51, 51, 51, 0.1);
            color: #00ff00;
            border-color: rgba(51, 51, 51, 0.2);
        }
        .footer-links a[href*="github"]:hover {
            background: #00ff00;
            color: #0a0a0a;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>🧪 Onion Service Connectivity Test</h1>
        <p><a href="/" class="back-link">← Back to Home</a> | <a href="/health" class="back-link">Health Status</a></p>
        
        <div class="test-box">
            <h3>🔍 Manual Connectivity Tests</h3>
            <p>These tests help diagnose onion service connectivity issues:</p>
            
            <button onclick="testTorProxy()" class="test-button">🌐 Test Tor Proxy</button>
            <button onclick="testDuckDuckGo()" class="test-button">🦆 Test DuckDuckGo Onion</button>
            <button onclick="testNNTPOnion()" class="test-button">📡 Test NNTP Onion Service</button>
            
            <div id="test-results"></div>
        </div>
        
        <div class="test-box">
            <h3>📊 Known Working Test Services (v3 Onion Addresses)</h3>
            <p>These onion services should work if your Tor is configured correctly:</p>
            <ul>
                <li><strong>DuckDuckGo:</strong> <code>duckduckgogg42xjoc72x3sjasowoarfbgcmvfimaftt6twagswzczad.onion</code></li>
                <li><strong>Facebook:</strong> <code>facebookwkhpilnemxj7asaniu7vnjjbiltxjqhye3mhbshg7kx5tfyd.onion</code></li>
                <li><strong>ProPublica:</strong> <code>p53lf57qovyuvwsc6xnrppddxpr23otqjafwze2xypdgc3gvhxoylvvid.onion</code></li>
            </ul>
            <p><em>⚠️ These are v3 onion addresses (56 characters) - v2 addresses (16 chars) are deprecated since 2021</em></p>
        </div>
        
        <div class="test-box">
            <h3>🎯 Target Servers</h3>
            <p><strong>Primary target:</strong> <code>peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion:119</code></p>
            <p><strong>Fallback server:</strong> <code>news.tcpreset.net:119</code> via Tor (anonymous access)</p>
            <p><strong>Connection method:</strong> All connections routed through Tor SOCKS5 proxy</p>
            
            <div style="margin-top: 15px; padding: 10px; background: #1a2a1a; border-radius: 5px;">
                <strong>ℹ️ Connection priority:</strong>
                <ol style="margin: 10px 0 0 20px;">
                    <li>First tries the onion service for maximum anonymity</li>
                    <li>If onion service fails, falls back to <code>news.tcpreset.net</code> via Tor</li>
                    <li>All traffic is encrypted and routed through Tor network</li>
                    <li>Your IP address is hidden from NNTP servers</li>
                </ol>
            </div>
            
            <div style="margin-top: 15px; padding: 10px; background: #2a1a00; border-radius: 5px;">
                <strong>⚠️ Common issues:</strong>
                <ul style="margin: 10px 0 0 20px;">
                    <li>Onion service temporarily offline (TTL expired)</li>
                    <li>Network connectivity issues</li>
                    <li>Tor circuit building problems</li>
                    <li>Fallback server configuration/access issues</li>
                </ul>
            </div>
        </div>
        
        <div class="test-box">
            <h3>📡 NNTP Protocol Information</h3>
            <p><strong>Protocol type:</strong> NNTP Client (newsreader) with <strong>Thread Support</strong></p>
            <p><strong>Commands used:</strong></p>
            <ul style="margin: 10px 0 0 20px; font-family: monospace; font-size: 0.9em;">
                <li><code>MODE READER</code> - Enter reader mode</li>
                <li><code>LIST</code> - Get newsgroup list</li>
                <li><code>GROUP &lt;name&gt;</code> - Select newsgroup</li>
                <li><code>XOVER &lt;range&gt;</code> - Get article overview</li>
                <li><code>ARTICLE &lt;id&gt;</code> - Retrieve article</li>
                <li><code>DATE</code>/<code>HELP</code> - Health check</li>
                <li><code>QUIT</code> - Close connection</li>
            </ul>
            <p><strong>Features:</strong> Thread-based article grouping using References headers</p>
            
            <div style="margin-top: 15px; padding: 10px; background: #1a2a1a; border-radius: 5px;">
                <strong>🧵 Threading Features:</strong>
                <ul style="margin: 10px 0 0 20px;">
                    <li>Automatic thread building using References headers</li>
                    <li>Subject-based fallback threading</li>
                    <li>Hierarchical reply display with indentation</li>
                    <li>Chronological sorting within threads</li>
                </ul>
            </div>
        </div>
        
        <div class="test-box">
            <h3>🔧 Command Line Tests</h3>
            <p>You can also test manually from command line:</p>
            <pre># Test Tor connectivity
curl --socks5 127.0.0.1:9050 http://duckduckgogg42xjoc72x3sjasowoarfbgcmvfimaftt6twagswzczad.onion

# Test NNTP onion service
timeout 120 curl --socks5 127.0.0.1:9050 -v \\
  telnet://peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion:119

# Test fallback server (anonymous access)
timeout 60 curl --socks5 127.0.0.1:9050 -v \\
  telnet://news.tcpreset.net:119</pre>
        </div>
        
        <div class="test-box">
            <h3>💡 Troubleshooting Tips</h3>
            <ul>
                <li>If Tor proxy test fails: Tor is not running or misconfigured</li>
                <li>If DuckDuckGo test fails: Network/Tor circuit issues</li>
                <li>If NNTP onion test fails: The specific NNTP service may be down</li>
                <li>Try again in a few minutes - onion services can be slow/unreliable</li>
                <li>Check Tor logs: <code>journalctl -u tor -f</code></li>
            </ul>
        </div>
        
        <footer class="app-footer">
            <div class="footer-links">
                <a href="https://github.com/gabrix73/onion-newsreader.git" target="_blank" rel="noopener">📁 Source Code</a>
                <a href="monero:44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L" onclick="fallbackCopyXMR(event)">⚡ Support Project</a>
                <a href="https://www.virebent.art" target="_blank" rel="noopener">🌐 virebent.art</a>
            </div>
        </footer>
    </div>
    
    <script>
        function showResult(type, message) {
            const resultsDiv = document.getElementById('test-results');
            const resultClass = type === 'success' ? 'result-success' : 
                               type === 'error' ? 'result-error' : 'result-info';
            resultsDiv.innerHTML += '<div class="result ' + resultClass + '">' + message + '</div>';
        }
        
        function showProgress(message) {
            const resultsDiv = document.getElementById('test-results');
            resultsDiv.innerHTML += '<div class="result result-info"><span class="spinner">🔄</span> ' + message + '</div>';
        }
        
        async function testTorProxy() {
            showProgress('Testing Tor proxy connectivity...');
            try {
                const response = await fetch('/health');
                const text = await response.text();
                if (text.includes('status-ok') || text.includes('Tor Proxy')) {
                    showResult('success', '✅ Tor proxy appears to be working');
                } else {
                    showResult('error', '❌ Tor proxy test inconclusive');
                }
            } catch (error) {
                showResult('error', '❌ Could not test Tor proxy: ' + error.message);
            }
        }
        
        async function testDuckDuckGo() {
            showProgress('Testing DuckDuckGo onion service (this may take 30-60 seconds)...');
            showResult('info', '💡 Manual test required - run: curl --socks5 127.0.0.1:9050 http://duckduckgogg42xjoc72x3sjasowoarfbgcmvfimaftt6twagswzczad.onion');
            showResult('info', '⚠️ Note: This is the current v3 onion address (v2 addresses are deprecated since 2021)');
        }
        
        async function testNNTPOnion() {
            showProgress('Testing NNTP onion service (this may take 2-3 minutes)...');
            try {
                const response = await fetch('/download-status');
                const data = await response.json();
                showResult('info', '📊 Current NNTP status: ' + data.message);
            } catch (error) {
                showResult('error', '❌ Could not check NNTP status: ' + error.message);
            }
            
            showResult('info', '💡 For full test, try downloading the active file from the main page');
        }

        function fallbackCopyXMR(event) {
            const xmrAddress = "44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L";
            
            if (!navigator.userAgent.includes('Monero')) {
                event.preventDefault();
                if (navigator.clipboard && navigator.clipboard.writeText) {
                    navigator.clipboard.writeText(xmrAddress).then(() => {
                        alert("✅ XMR address copied to clipboard!");
                    }).catch(() => {
                        prompt("Copy XMR address:", xmrAddress);
                    });
                } else {
                    prompt("Copy XMR address:", xmrAddress);
                }
            }
        }
    </script>
</body>
</html>`
	
	w.Write([]byte(testHTML))
}

func (s *NewsServer) handleHome(w http.ResponseWriter, r *http.Request) {
	s.mutex.RLock()
	groupCount := len(s.groups)
	s.mutex.RUnlock()
	
	connectionStatus := "unknown"
	torStatus := "unknown"
	
	if err := s.testTorProxy(); err != nil {
		torStatus = "failed"
		connectionStatus = "tor-proxy-failed"
	} else {
		torStatus = "ok"
		if err := s.client.HealthCheck(); err != nil {
			connectionStatus = "nntp-disconnected"
		} else {
			connectionStatus = "connected"
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	homeHTML := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>🧅 Onion Newsreader</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body { 
            font-family: 'Monaco', 'Menlo', 'Ubuntu Mono', monospace; 
            background: #0a0a0a; 
            color: #00ff00; 
            line-height: 1.6;
        }
        .container { max-width: 1200px; margin: 0 auto; padding: 20px; }
        .header { 
            background: #1a1a1a; 
            padding: 20px; 
            border-radius: 8px; 
            margin-bottom: 20px;
            border: 1px solid #333;
        }
        .header h1 { color: #00ff00; margin-bottom: 10px; }
        .header p { color: #888; }
        .status { 
            background: #1a1a1a; 
            padding: 15px; 
            border-radius: 8px; 
            margin-bottom: 20px;
            border: 1px solid #333;
        }
        .search-box { 
            background: #1a1a1a; 
            padding: 20px; 
            border-radius: 8px; 
            margin-bottom: 20px;
            border: 1px solid #333;
        }
        .search-box input { 
            width: 100%; 
            padding: 12px; 
            background: #0a0a0a; 
            border: 1px solid #333; 
            color: #00ff00; 
            border-radius: 4px;
            font-family: monospace;
        }
        .search-box button { 
            padding: 12px 20px; 
            background: #333; 
            border: none; 
            color: #00ff00; 
            border-radius: 4px; 
            cursor: pointer; 
            margin-top: 10px;
            font-family: monospace;
        }
        .search-box button:hover { background: #555; }
        .actions { 
            display: grid; 
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); 
            gap: 15px; 
            margin-bottom: 20px;
        }
        .action-card { 
            background: #1a1a1a; 
            padding: 20px; 
            border-radius: 8px; 
            border: 1px solid #333;
            text-align: center;
        }
        .action-card a { 
            color: #00aaff; 
            text-decoration: none; 
            font-weight: bold;
        }
        .action-card a:hover { color: #00ff00; }
        .footer { 
            text-align: center; 
            color: #666; 
            margin-top: 40px; 
            padding: 20px;
        }
        .status-good { color: #00ff00; }
        .status-warn { color: #ffaa00; }
        .status-error { color: #ff0000; }
        .status-unknown { color: #888; }
        .alert { padding: 15px; border-radius: 8px; margin-bottom: 20px; }
        .alert-error { background: #2a1a1a; border: 1px solid #ff8800; color: #ff8800; }
        .alert-warning { background: #2a2a1a; border: 1px solid #ffaa00; color: #ffaa00; }
        .alert-info { background: #1a2a2a; border: 1px solid #00aaff; color: #00aaff; }
        .app-footer {
            margin-top: 40px;
            padding: 25px 0;
            border-top: 2px solid #333;
            background: #1a1a1a;
            text-align: center;
        }
        .footer-links {
            display: flex;
            justify-content: center;
            gap: 25px;
            flex-wrap: wrap;
        }
        .footer-links a {
            color: #00aaff;
            text-decoration: none;
            font-weight: 500;
            padding: 8px 15px;
            border-radius: 20px;
            background: rgba(0, 170, 255, 0.1);
            transition: all 0.3s ease;
            border: 1px solid rgba(0, 170, 255, 0.2);
        }
        .footer-links a:hover {
            background: #00aaff;
            color: #0a0a0a;
        }
        .footer-links a[href^="monero:"] {
            background: rgba(255, 102, 0, 0.1);
            color: #ff6600;
            border-color: rgba(255, 102, 0, 0.2);
        }
        .footer-links a[href^="monero:"]:hover {
            background: #ff6600;
            color: #0a0a0a;
        }
        .footer-links a[href*="github"] {
            background: rgba(51, 51, 51, 0.1);
            color: #00ff00;
            border-color: rgba(51, 51, 51, 0.2);
        }
        .footer-links a[href*="github"]:hover {
            background: #00ff00;
            color: #0a0a0a;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>🧅 Onion Newsreader</h1>
            <p>Anonymous NNTP Client via Tor • Secure Newsgroup Access • Thread Support</p>
        </div>

        {{if eq .ConnectionStatus "tor-proxy-failed"}}
        <div class="alert alert-error">
            <h3>❌ Tor Connection Failed</h3>
            <p>Cannot connect to Tor proxy at 127.0.0.1:9050</p>
            <p>Please ensure Tor is installed and running: <code>systemctl start tor</code></p>
        </div>
        {{else if eq .ConnectionStatus "nntp-disconnected"}}
        <div class="alert alert-warning">
            <h3>⚠️ NNTP Services Unreachable</h3>
            <p>Tor proxy is working but neither the onion service nor fallback server are reachable.</p>
            <p><strong>Primary target:</strong> <code>peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion</code></p>
            <p><strong>Fallback server:</strong> <code>news.tcpreset.net</code> via Tor (anonymous access)</p>
            <p>This may be temporary - services might be down or experiencing high load.</p>
            <p><a href="/test-onion" style="color: #00aaff;">🧪 Run diagnostic tests</a> to troubleshoot.</p>
        </div>
        {{else if eq .ConnectionStatus "connected"}}
        <div class="alert alert-info">
            <h3>✅ All Systems Operational</h3>
            <p>Tor proxy and NNTP service are both reachable.</p>
        </div>
        {{end}}

        <div class="status">
            <h3>📊 Status</h3>
            <p>📡 Primary: <code>peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion</code></p>
            <p>🔄 Fallback: <code>news.tcpreset.net</code> via Tor (anonymous access)</p>
            <p>🌐 Tor Proxy: <span class="status-{{.TorStatusClass}}">{{.TorStatus}}</span></p>
            <p>📡 NNTP: <span class="status-{{.ConnectionStatusClass}}">{{.ConnectionStatus}}</span></p>
            <p>📊 Cached Groups: <span class="status-good">{{.GroupCount}}</span></p>
            <p>🧵 Threading: <span class="status-good">Enabled</span></p>
            <p>🔗 Session: <code>{{.Session}}</code></p>
            <p>⚡ <a href="/health" style="color: #00aaff;">Detailed Health Check</a></p>
        </div>

        {{if ne .ConnectionStatus "tor-proxy-failed"}}
        <div class="search-box">
            <h3>🔍 Search Newsgroups</h3>
            <form action="/search" method="get">
                <input type="text" name="q" placeholder="Search newsgroups (e.g., comp.*, alt.*, *.linux.*)" required>
                <button type="submit">Search Groups</button>
            </form>
        </div>

        <div class="actions">
            <div class="action-card">
                <h4>📋 Browse Groups</h4>
                <p><a href="/active">View All Groups</a></p>
                <p>Browse available newsgroups</p>
            </div>

            <div class="action-card">
                <h4>🔄 Refresh</h4>
                <p><a href="/active?refresh=1">Refresh Group List</a></p>
                <p>Update newsgroup cache</p>
            </div>

            <div class="action-card">
                <h4>📊 System Info</h4>
                <p><a href="/health">Health Status</a></p>
                <p>Connection and system status</p>
            </div>
            
            <div class="action-card">
                <h4>🧪 Diagnostics</h4>
                <p><a href="/test-onion">Test Onion Services</a></p>
                <p>Connectivity troubleshooting</p>
            </div>

            <div class="action-card">
                <h4>🔍 Quick Search</h4>
                <p><a href="/search?q=comp.*">comp.*</a> | <a href="/search?q=alt.*">alt.*</a></p>
                <p><a href="/search?q=*linux*">*linux*</a> | <a href="/search?q=sci.*">sci.*</a></p>
                <p>Popular newsgroup categories</p>
            </div>
        </div>
        {{else}}
        <div class="alert alert-error">
            <h3>🔧 Troubleshooting Steps</h3>
            <ol>
                <li>Install Tor: <code>apt-get install tor</code></li>
                <li>Start Tor service: <code>systemctl start tor</code></li>
                <li>Enable Tor service: <code>systemctl enable tor</code></li>
                <li>Check Tor status: <code>systemctl status tor</code></li>
                <li>Refresh this page after fixing Tor</li>
            </ol>
        </div>
        {{end}}

        <div class="footer">
            <p>🔒 All connections are routed through Tor for maximum anonymity</p>
            <p>🧅 Primary: Onion service • 🔄 Fallback: news.tcpreset.net via Tor (anonymous)</p>
            <p>🧵 Thread-based article organization for better discussion flow</p>
            <p>✍️ Posting URL: <code>http://itcxzfm2h36hfj6j7qxksyfm4ipp3co4rkl62sgge7hp6u77lbretiyd.onion:8880/</code></p>
        </div>
        
        <footer class="app-footer">
            <div class="footer-links">
                <a href="https://github.com/gabrix73/onion-newsreader.git" target="_blank" rel="noopener">📁 Source Code</a>
                <a href="monero:44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L" onclick="fallbackCopyXMR(event)">⚡ Support Project</a>
                <a href="https://www.virebent.art" target="_blank" rel="noopener">🌐 virebent.art</a>
            </div>
        </footer>
    </div>
    
    <script>
    function fallbackCopyXMR(event) {
        const xmrAddress = "44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L";
        
        if (!navigator.userAgent.includes('Monero')) {
            event.preventDefault();
            if (navigator.clipboard && navigator.clipboard.writeText) {
                navigator.clipboard.writeText(xmrAddress).then(() => {
                    alert("✅ XMR address copied to clipboard!");
                }).catch(() => {
                    prompt("Copy XMR address:", xmrAddress);
                });
            } else {
                prompt("Copy XMR address:", xmrAddress);
            }
        }
    }
    </script>
</body>
</html>`

	tmpl, _ := template.New("home").Parse(homeHTML)
	
	torStatusClass := "unknown"
	connectionStatusClass := "unknown"
	
	switch torStatus {
	case "ok":
		torStatusClass = "good"
	case "failed":
		torStatusClass = "error"
	}
	
	switch connectionStatus {
	case "connected":
		connectionStatusClass = "good"
	case "nntp-disconnected":
		connectionStatusClass = "warn"
	case "tor-proxy-failed":
		connectionStatusClass = "error"
	}
	
	data := map[string]interface{}{
		"GroupCount":             groupCount,
		"Session":                s.session,
		"ConnectionStatus":       connectionStatus,
		"ConnectionStatusClass":  connectionStatusClass,
		"TorStatus":             torStatus,
		"TorStatusClass":        torStatusClass,
	}
	tmpl.Execute(w, data)
}

func (s *NewsServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	
	if query == "" {
		// Show search form
		searchFormHTML := `<!DOCTYPE html>
<html>
<head>
    <title>🔍 Search Newsgroups - Onion Newsreader</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #00ff00; padding: 20px; }
        .container { max-width: 800px; margin: 0 auto; }
        .search-box { background: #1a1a1a; padding: 20px; border-radius: 8px; margin: 20px 0; border: 1px solid #333; }
        .search-box input { width: 100%; padding: 12px; background: #0a0a0a; border: 1px solid #333; color: #00ff00; border-radius: 4px; font-family: monospace; }
        .search-box button { padding: 12px 20px; background: #333; border: none; color: #00ff00; border-radius: 4px; cursor: pointer; margin-top: 10px; font-family: monospace; }
        .search-box button:hover { background: #555; }
        .back-link { color: #00aaff; text-decoration: none; }
        .back-link:hover { color: #00ff00; }
        .examples { background: #1a1a1a; padding: 15px; border-radius: 8px; margin: 20px 0; border: 1px solid #333; }
        .examples ul { margin: 10px 0 0 20px; }
        .examples li { margin: 5px 0; }
        .examples code { background: #0a0a0a; padding: 2px 4px; border-radius: 2px; }
        .app-footer {
            margin-top: 40px;
            padding: 25px 0;
            border-top: 2px solid #333;
            background: #1a1a1a;
            text-align: center;
        }
        .footer-links {
            display: flex;
            justify-content: center;
            gap: 25px;
            flex-wrap: wrap;
        }
        .footer-links a {
            color: #00aaff;
            text-decoration: none;
            font-weight: 500;
            padding: 8px 15px;
            border-radius: 20px;
            background: rgba(0, 170, 255, 0.1);
            transition: all 0.3s ease;
            border: 1px solid rgba(0, 170, 255, 0.2);
        }
        .footer-links a:hover {
            background: #00aaff;
            color: #0a0a0a;
        }
        .footer-links a[href^="monero:"] {
            background: rgba(255, 102, 0, 0.1);
            color: #ff6600;
            border-color: rgba(255, 102, 0, 0.2);
        }
        .footer-links a[href^="monero:"]:hover {
            background: #ff6600;
            color: #0a0a0a;
        }
        .footer-links a[href*="github"] {
            background: rgba(51, 51, 51, 0.1);
            color: #00ff00;
            border-color: rgba(51, 51, 51, 0.2);
        }
        .footer-links a[href*="github"]:hover {
            background: #00ff00;
            color: #0a0a0a;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>🔍 Search Newsgroups</h1>
        <p><a href="/" class="back-link">← Back to Home</a></p>
        
        <div class="search-box">
            <h3>Enter Search Query</h3>
            <form action="/search" method="get">
                <input type="text" name="q" placeholder="Search newsgroups (e.g., comp.*, alt.*, *.linux.*)" autofocus>
                <button type="submit">🔍 Search</button>
            </form>
        </div>
        
        <div class="examples">
            <h3>📝 Search Examples</h3>
            <ul>
                <li><code>comp.*</code> - All computer-related groups</li>
                <li><code>alt.*</code> - All alternative groups</li>
                <li><code>*.linux.*</code> - All Linux-related groups</li>
                <li><code>rec.sport.*</code> - All recreation sport groups</li>
                <li><code>*security*</code> - All groups containing "security"</li>
                <li><code>news.*</code> - All news groups</li>
                <li><code>sci.*</code> - All science groups</li>
                <li><code>soc.*</code> - All social groups</li>
            </ul>
            <p><strong>Wildcards:</strong> Use <code>*</code> to match any characters</p>
            <p><strong>Case:</strong> Search is case-insensitive</p>
        </div>
        
        <footer class="app-footer">
            <div class="footer-links">
                <a href="https://github.com/gabrix73/onion-newsreader.git" target="_blank" rel="noopener">📁 Source Code</a>
                <a href="monero:44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L" onclick="fallbackCopyXMR(event)">⚡ Support Project</a>
                <a href="https://www.virebent.art" target="_blank" rel="noopener">🌐 virebent.art</a>
            </div>
        </footer>
    </div>
    
    <script>
    function fallbackCopyXMR(event) {
        const xmrAddress = "44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L";
        
        if (!navigator.userAgent.includes('Monero')) {
            event.preventDefault();
            if (navigator.clipboard && navigator.clipboard.writeText) {
                navigator.clipboard.writeText(xmrAddress).then(() => {
                    alert("✅ XMR address copied to clipboard!");
                }).catch(() => {
                    prompt("Copy XMR address:", xmrAddress);
                });
            } else {
                prompt("Copy XMR address:", xmrAddress);
            }
        }
    }
    </script>
</body>
</html>`
		w.Write([]byte(searchFormHTML))
		return
	}
	
	// Perform search
	s.mutex.RLock()
	allGroups := s.groups
	groupCount := len(allGroups)
	s.mutex.RUnlock()
	
	if groupCount == 0 {
		noGroupsHTML := `<!DOCTYPE html>
<html>
<head>
    <title>🔍 Search Results - Onion Newsreader</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #00ff00; padding: 20px; }
        .container { max-width: 800px; margin: 0 auto; }
        .back-link { color: #00aaff; text-decoration: none; }
        .back-link:hover { color: #00ff00; }
        .warning { background: #2a1a00; border: 1px solid #ffaa00; color: #ffaa00; padding: 15px; border-radius: 8px; margin: 20px 0; }
        .app-footer {
            margin-top: 40px;
            padding: 25px 0;
            border-top: 2px solid #333;
            background: #1a1a1a;
            text-align: center;
        }
        .footer-links {
            display: flex;
            justify-content: center;
            gap: 25px;
            flex-wrap: wrap;
        }
        .footer-links a {
            color: #00aaff;
            text-decoration: none;
            font-weight: 500;
            padding: 8px 15px;
            border-radius: 20px;
            background: rgba(0, 170, 255, 0.1);
            transition: all 0.3s ease;
            border: 1px solid rgba(0, 170, 255, 0.2);
        }
        .footer-links a:hover {
            background: #00aaff;
            color: #0a0a0a;
        }
        .footer-links a[href^="monero:"] {
            background: rgba(255, 102, 0, 0.1);
            color: #ff6600;
            border-color: rgba(255, 102, 0, 0.2);
        }
        .footer-links a[href^="monero:"]:hover {
            background: #ff6600;
            color: #0a0a0a;
        }
        .footer-links a[href*="github"] {
            background: rgba(51, 51, 51, 0.1);
            color: #00ff00;
            border-color: rgba(51, 51, 51, 0.2);
        }
        .footer-links a[href*="github"]:hover {
            background: #00ff00;
            color: #0a0a0a;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>🔍 Search Results</h1>
        <p><a href="/" class="back-link">← Back to Home</a> | <a href="/search" class="back-link">🔍 New Search</a></p>
        
        <div class="warning">
            <h3>⚠️ No Groups Available</h3>
            <p>No newsgroups are currently cached. You need to download the active file first.</p>
            <p><a href="/active?refresh=1" style="color: #00aaff;">📥 Download Active File</a></p>
        </div>
        
        <footer class="app-footer">
            <div class="footer-links">
                <a href="https://github.com/gabrix73/onion-newsreader.git" target="_blank" rel="noopener">📁 Source Code</a>
                <a href="monero:44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L" onclick="fallbackCopyXMR(event)">⚡ Support Project</a>
                <a href="https://www.virebent.art" target="_blank" rel="noopener">🌐 virebent.art</a>
            </div>
        </footer>
    </div>
    
    <script>
    function fallbackCopyXMR(event) {
        const xmrAddress = "44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L";
        
        if (!navigator.userAgent.includes('Monero')) {
            event.preventDefault();
            if (navigator.clipboard && navigator.clipboard.writeText) {
                navigator.clipboard.writeText(xmrAddress).then(() => {
                    alert("✅ XMR address copied to clipboard!");
                }).catch(() => {
                    prompt("Copy XMR address:", xmrAddress);
                });
            } else {
                prompt("Copy XMR address:", xmrAddress);
            }
        }
    }
    </script>
</body>
</html>`
		w.Write([]byte(noGroupsHTML))
		return
	}
	
	// Convert search pattern to regex
	pattern := strings.ToLower(query)
	pattern = strings.ReplaceAll(pattern, ".", "\\.")
	pattern = strings.ReplaceAll(pattern, "*", ".*")
	pattern = "^" + pattern + "$"
	
	log.Printf("🔍 Searching for pattern: %s (regex: %s)", query, pattern)
	
	var matches []*NewsgroupInfo
	for _, group := range allGroups {
		if matched, _ := regexp.MatchString(pattern, strings.ToLower(group.Name)); matched {
			matches = append(matches, group)
		}
	}
	
	// Sort results
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Name < matches[j].Name
	})
	
	// Limit results to prevent huge pages
	maxResults := 500
	truncated := false
	if len(matches) > maxResults {
		matches = matches[:maxResults]
		truncated = true
	}
	
	log.Printf("🔍 Search completed: %d matches for '%s'", len(matches), query)
	
	searchResultsHTML := `<!DOCTYPE html>
<html>
<head>
    <title>🔍 Search Results - Onion Newsreader</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #00ff00; padding: 20px; }
        .container { max-width: 1000px; margin: 0 auto; }
        .search-header { background: #1a1a1a; padding: 20px; border-radius: 8px; margin-bottom: 20px; border: 1px solid #333; }
        .search-box { margin: 15px 0; }
        .search-box input { width: 70%; padding: 8px; background: #0a0a0a; border: 1px solid #333; color: #00ff00; border-radius: 4px; font-family: monospace; }
        .search-box button { padding: 8px 16px; background: #333; border: none; color: #00ff00; border-radius: 4px; cursor: pointer; margin-left: 10px; }
        .search-box button:hover { background: #555; }
        .results-summary { background: #1a1a1a; padding: 15px; border-radius: 8px; margin-bottom: 20px; border: 1px solid #333; }
        .group-list { background: #1a1a1a; padding: 20px; border-radius: 8px; border: 1px solid #333; }
        .group-item { padding: 8px; margin: 3px 0; background: #0a0a0a; border-radius: 3px; }
        .group-name { color: #00aaff; }
        .group-stats { color: #888; font-size: 0.9em; }
        .back-link { color: #00aaff; text-decoration: none; }
        .back-link:hover { color: #00ff00; }
        .warning { background: #2a1a00; border: 1px solid #ffaa00; color: #ffaa00; padding: 15px; border-radius: 8px; margin: 20px 0; }
        .info { background: #1a2a2a; border: 1px solid #0088aa; color: #0088aa; padding: 15px; border-radius: 8px; margin: 20px 0; }
        .app-footer {
            margin-top: 40px;
            padding: 25px 0;
            border-top: 2px solid #333;
            background: #1a1a1a;
            text-align: center;
        }
        .footer-links {
            display: flex;
            justify-content: center;
            gap: 25px;
            flex-wrap: wrap;
        }
        .footer-links a {
            color: #00aaff;
            text-decoration: none;
            font-weight: 500;
            padding: 8px 15px;
            border-radius: 20px;
            background: rgba(0, 170, 255, 0.1);
            transition: all 0.3s ease;
            border: 1px solid rgba(0, 170, 255, 0.2);
        }
        .footer-links a:hover {
            background: #00aaff;
            color: #0a0a0a;
        }
        .footer-links a[href^="monero:"] {
            background: rgba(255, 102, 0, 0.1);
            color: #ff6600;
            border-color: rgba(255, 102, 0, 0.2);
        }
        .footer-links a[href^="monero:"]:hover {
            background: #ff6600;
            color: #0a0a0a;
        }
        .footer-links a[href*="github"] {
            background: rgba(51, 51, 51, 0.1);
            color: #00ff00;
            border-color: rgba(51, 51, 51, 0.2);
        }
        .footer-links a[href*="github"]:hover {
            background: #00ff00;
            color: #0a0a0a;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="search-header">
            <h1>🔍 Search Results</h1>
            <p><a href="/" class="back-link">← Back to Home</a> | <a href="/search" class="back-link">🔍 New Search</a> | <a href="/active" class="back-link">📋 All Groups</a></p>
            
            <div class="search-box">
                <form action="/search" method="get">
                    <input type="text" name="q" value="{{.Query}}" placeholder="Search newsgroups...">
                    <button type="submit">🔍 Search</button>
                </form>
            </div>
        </div>

        <div class="results-summary">
            <h3>📊 Search Results</h3>
            <p><strong>Query:</strong> <code>{{.Query}}</code></p>
            <p><strong>Matches:</strong> {{.MatchCount}} out of {{.TotalGroups}} total groups</p>
            {{if .Truncated}}
            <p><strong>⚠️ Results limited to first {{.MaxResults}} matches - try a more specific search</strong></p>
            {{end}}
        </div>

        {{if eq .MatchCount 0}}
        <div class="info">
            <h3>ℹ️ No Matches Found</h3>
            <p>No newsgroups match your search pattern: <code>{{.Query}}</code></p>
            <p><strong>Tips:</strong></p>
            <ul>
                <li>Try using wildcards: <code>comp.*</code>, <code>*linux*</code>, <code>alt.*</code></li>
                <li>Check spelling and case (search is case-insensitive)</li>
                <li>Use broader patterns: <code>*security*</code> instead of <code>comp.security.*</code></li>
            </ul>
        </div>
        {{else}}
        <div class="group-list">
            <h3>📋 Matching Groups</h3>
            {{range .Groups}}
            <div class="group-item">
                <div class="group-name">
                    <a href="/group/{{.Name}}" style="color: #00aaff; text-decoration: none;">{{.Name}}</a>
                </div>
                <div class="group-stats">Range: {{.Low}}-{{.High}} ({{.Status}}) • Articles: {{.ArticleCount}}</div>
            </div>
            {{end}}
        </div>
        {{end}}
        
        <footer class="app-footer">
            <div class="footer-links">
                <a href="https://github.com/gabrix73/onion-newsreader.git" target="_blank" rel="noopener">📁 Source Code</a>
                <a href="monero:44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L" onclick="fallbackCopyXMR(event)">⚡ Support Project</a>
                <a href="https://www.virebent.art" target="_blank" rel="noopener">🌐 virebent.art</a>
            </div>
        </footer>
    </div>
    
    <script>
    function fallbackCopyXMR(event) {
        const xmrAddress = "44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L";
        
        if (!navigator.userAgent.includes('Monero')) {
            event.preventDefault();
            if (navigator.clipboard && navigator.clipboard.writeText) {
                navigator.clipboard.writeText(xmrAddress).then(() => {
                    alert("✅ XMR address copied to clipboard!");
                }).catch(() => {
                    prompt("Copy XMR address:", xmrAddress);
                });
            } else {
                prompt("Copy XMR address:", xmrAddress);
            }
        }
    }
    </script>
</body>
</html>`

	tmpl, _ := template.New("search").Parse(searchResultsHTML)
	
	// Calculate article counts for display
	for _, group := range matches {
		if group.High > group.Low {
			group.ArticleCount = group.High - group.Low + 1
		} else {
			group.ArticleCount = 0
		}
	}
	
	data := map[string]interface{}{
		"Query":       query,
		"Groups":      matches,
		"MatchCount":  len(matches),
		"TotalGroups": groupCount,
		"Truncated":   truncated,
		"MaxResults":  maxResults,
	}
	tmpl.Execute(w, data)
}

func (s *NewsServer) handleGroup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	
	// Extract group name from URL path
	path := strings.TrimPrefix(r.URL.Path, "/group/")
	if path == "" {
		http.Redirect(w, r, "/active", http.StatusSeeOther)
		return
	}
	
	groupName := path
	log.Printf("🔍 Viewing group: %s", groupName)
	
	// Check if group exists in cache
	s.mutex.RLock()
	groupInfo, exists := s.groups[groupName]
	s.mutex.RUnlock()
	
	if !exists {
		notFoundHTML := `<!DOCTYPE html>
<html>
<head>
    <title>❌ Group Not Found - Onion Newsreader</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #00ff00; padding: 20px; }
        .container { max-width: 800px; margin: 0 auto; }
        .back-link { color: #00aaff; text-decoration: none; }
        .back-link:hover { color: #00ff00; }
        .error { background: #2a1a1a; border: 1px solid #aa0000; color: #aa0000; padding: 15px; border-radius: 8px; margin: 20px 0; }
        .app-footer {
            margin-top: 40px;
            padding: 25px 0;
            border-top: 2px solid #333;
            background: #1a1a1a;
            text-align: center;
        }
        .footer-links {
            display: flex;
            justify-content: center;
            gap: 25px;
            flex-wrap: wrap;
        }
        .footer-links a {
            color: #00aaff;
            text-decoration: none;
            font-weight: 500;
            padding: 8px 15px;
            border-radius: 20px;
            background: rgba(0, 170, 255, 0.1);
            transition: all 0.3s ease;
            border: 1px solid rgba(0, 170, 255, 0.2);
        }
        .footer-links a:hover {
            background: #00aaff;
            color: #0a0a0a;
        }
        .footer-links a[href^="monero:"] {
            background: rgba(255, 102, 0, 0.1);
            color: #ff6600;
            border-color: rgba(255, 102, 0, 0.2);
        }
        .footer-links a[href^="monero:"]:hover {
            background: #ff6600;
            color: #0a0a0a;
        }
        .footer-links a[href*="github"] {
            background: rgba(51, 51, 51, 0.1);
            color: #00ff00;
            border-color: rgba(51, 51, 51, 0.2);
        }
        .footer-links a[href*="github"]:hover {
            background: #00ff00;
            color: #0a0a0a;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>❌ Newsgroup Not Found</h1>
        <p><a href="/" class="back-link">← Back to Home</a> | <a href="/active" class="back-link">📋 All Groups</a></p>
        
        <div class="error">
            <h3>Group Not Available</h3>
            <p>The newsgroup <strong>` + groupName + `</strong> was not found in the cached group list.</p>
            <p>This could mean:</p>
            <ul>
                <li>The group name is misspelled</li>
                <li>The group doesn't exist on this server</li>
                <li>The active file cache needs to be refreshed</li>
            </ul>
            <p><a href="/active?refresh=1" style="color: #00aaff;">🔄 Refresh Group Cache</a></p>
        </div>
        
        <footer class="app-footer">
            <div class="footer-links">
                <a href="https://github.com/gabrix73/onion-newsreader.git" target="_blank" rel="noopener">📁 Source Code</a>
                <a href="monero:44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L" onclick="fallbackCopyXMR(event)">⚡ Support Project</a>
                <a href="https://www.virebent.art" target="_blank" rel="noopener">🌐 virebent.art</a>
            </div>
        </footer>
    </div>
    
    <script>
    function fallbackCopyXMR(event) {
        const xmrAddress = "44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L";
        
        if (!navigator.userAgent.includes('Monero')) {
            event.preventDefault();
            if (navigator.clipboard && navigator.clipboard.writeText) {
                navigator.clipboard.writeText(xmrAddress).then(() => {
                    alert("✅ XMR address copied to clipboard!");
                }).catch(() => {
                    prompt("Copy XMR address:", xmrAddress);
                });
            } else {
                prompt("Copy XMR address:", xmrAddress);
            }
        }
    }
    </script>
</body>
</html>`
		w.Write([]byte(notFoundHTML))
		return
	}
	
	// Try to get articles for this group and build threads
	var threads []*Thread
	var articleCount, low, high int
	var connectionError string
	
	if err := s.client.ensureConnected(); err != nil {
		connectionError = fmt.Sprintf("Connection failed: %v", err)
	} else {
		var err error
		articleCount, low, high, err = s.client.SelectGroup(groupName)
		if err != nil {
			connectionError = fmt.Sprintf("Failed to select group: %v", err)
		} else {
			maxArticles := 200  // Increased for better threading
			articles, err := s.client.GetArticleOverview(low, high, maxArticles)
			if err != nil {
				connectionError = fmt.Sprintf("Failed to get articles: %v", err)
			} else {
				// Build threads from articles
				threads = buildThreads(articles)
			}
		}
	}
	
	groupHTML := `<!DOCTYPE html>
<html>
<head>
    <title>📰 {{.GroupName}} - Onion Newsreader</title>
    <meta charset="utf-8">
    <style>
        body { font-family: monospace; background: #0a0a0a; color: #00ff00; padding: 20px; }
        .container { max-width: 1200px; margin: 0 auto; }
        .header { background: #1a1a1a; padding: 20px; border-radius: 8px; margin-bottom: 20px; border: 1px solid #333; }
        .group-info { background: #1a1a1a; padding: 15px; border-radius: 8px; margin-bottom: 20px; border: 1px solid #333; }
        .thread-list { background: #1a1a1a; padding: 20px; border-radius: 8px; border: 1px solid #333; }
        .thread-item { 
            margin: 10px 0; 
            padding: 15px; 
            background: #0a0a0a; 
            border-radius: 5px; 
            border-left: 4px solid #333; 
        }
        .thread-item:hover { 
            background: #1a1a0a; 
            border-left-color: #ffaa00; 
        }
        .thread-subject { 
            color: #00aaff; 
            font-weight: bold; 
            margin-bottom: 8px; 
        }
        .thread-subject a { 
            color: #00aaff; 
            text-decoration: none; 
        }
        .thread-subject a:hover { 
            color: #00ff00; 
        }
        .thread-meta { 
            color: #888; 
            font-size: 0.9em; 
            margin-bottom: 10px;
        }
        .thread-replies { 
            margin-left: 20px; 
            border-left: 2px solid #333; 
            padding-left: 15px; 
        }
        .reply-item { 
            margin: 5px 0; 
            padding: 8px; 
            background: #1a1a1a; 
            border-radius: 3px; 
            font-size: 0.9em;
        }
        .reply-subject { 
            color: #00aaff; 
        }
        .reply-subject a { 
            color: #00aaff; 
            text-decoration: none; 
        }
        .reply-subject a:hover { 
            color: #00ff00; 
        }
        .reply-meta { 
            color: #666; 
            font-size: 0.8em; 
        }
        .back-link { color: #00aaff; text-decoration: none; }
        .back-link:hover { color: #00ff00; }
        .error { background: #2a1a1a; border: 1px solid #aa0000; color: #aa0000; padding: 15px; border-radius: 8px; margin: 20px 0; }
        .warning { background: #2a1a00; border: 1px solid #ffaa00; color: #ffaa00; padding: 15px; border-radius: 8px; margin: 20px 0; }
        .info { background: #1a2a2a; border: 1px solid #0088aa; color: #0088aa; padding: 15px; border-radius: 8px; margin: 20px 0; }
        .refresh-btn { background: #333; color: #00ff00; border: none; padding: 8px 16px; border-radius: 4px; cursor: pointer; margin: 10px 5px; }
        .refresh-btn:hover { background: #555; }
        .stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 10px; }
        .stat-item { background: #0a0a0a; padding: 10px; border-radius: 4px; text-align: center; }
        .posting-section { background: #1a1a1a; padding: 20px; border-radius: 8px; margin-bottom: 20px; border: 1px solid #333; text-align: center; }
        .post-button { 
            display: inline-block; 
            background: #2a4a2a; 
            color: #00ff00; 
            text-decoration: none; 
            padding: 12px 24px; 
            border-radius: 6px; 
            font-weight: bold; 
            margin: 10px 0; 
            border: 2px solid #00aa00;
            transition: all 0.3s ease;
        }
        .post-button:hover { 
            background: #3a6a3a; 
            border-color: #00ff00; 
            box-shadow: 0 0 10px rgba(0, 255, 0, 0.3);
        }
        .posting-note { 
            font-size: 0.9em; 
            color: #888; 
            margin-top: 15px; 
            text-align: left; 
            background: #0a0a0a; 
            padding: 10px; 
            border-radius: 4px; 
            border-left: 3px solid #ffaa00;
        }
        .thread-count {
            color: #ffaa00;
            font-weight: bold;
        }
        .app-footer {
            margin-top: 40px;
            padding: 25px 0;
            border-top: 2px solid #333;
            background: #1a1a1a;
            text-align: center;
        }
        .footer-links {
            display: flex;
            justify-content: center;
            gap: 25px;
            flex-wrap: wrap;
        }
        .footer-links a {
            color: #00aaff;
            text-decoration: none;
            font-weight: 500;
            padding: 8px 15px;
            border-radius: 20px;
            background: rgba(0, 170, 255, 0.1);
            transition: all 0.3s ease;
            border: 1px solid rgba(0, 170, 255, 0.2);
        }
        .footer-links a:hover {
            background: #00aaff;
            color: #0a0a0a;
        }
        .footer-links a[href^="monero:"] {
            background: rgba(255, 102, 0, 0.1);
            color: #ff6600;
            border-color: rgba(255, 102, 0, 0.2);
        }
        .footer-links a[href^="monero:"]:hover {
            background: #ff6600;
            color: #0a0a0a;
        }
        .footer-links a[href*="github"] {
            background: rgba(51, 51, 51, 0.1);
            color: #00ff00;
            border-color: rgba(51, 51, 51, 0.2);
        }
        .footer-links a[href*="github"]:hover {
            background: #00ff00;
            color: #0a0a0a;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>📰 {{.GroupName}}</h1>
            <p><a href="/" class="back-link">← Back to Home</a> | 
               <a href="/active" class="back-link">📋 All Groups</a> | 
               <a href="/search?q={{.GroupPattern}}" class="back-link">🔍 Similar Groups</a></p>
        </div>

        <div class="group-info">
            <h3>📊 Group Information</h3>
            <div class="stats">
                <div class="stat-item">
                    <strong>📝 Cached Range:</strong><br>{{.CachedLow}} - {{.CachedHigh}}
                </div>
                <div class="stat-item">
                    <strong>📊 Cached Count:</strong><br>{{.CachedCount}}
                </div>
                {{if .LiveStats}}
                <div class="stat-item">
                    <strong>📡 Live Range:</strong><br>{{.LiveLow}} - {{.LiveHigh}}
                </div>
                <div class="stat-item">
                    <strong>🔢 Live Count:</strong><br>{{.LiveCount}}
                </div>
                {{end}}
                <div class="stat-item">
                    <strong>📋 Status:</strong><br>{{.Status}}
                </div>
                <div class="stat-item">
                    <strong>🧵 Threads:</strong><br><span class="thread-count">{{.ThreadCount}}</span>
                </div>
            </div>
        </div>

        <div class="posting-section">
            <h3>✍️ Post to Newsgroup</h3>
            <p>Create a new post in <strong>{{.GroupName}}</strong></p>
            <a href="http://itcxzfm2h36hfj6j7qxksyfm4ipp3co4rkl62sgge7hp6u77lbretiyd.onion:8880/?newsgroups={{.EncodedGroupName}}" target="_blank" class="post-button">
                📝 CREATE NEW POST
            </a>
            <p class="posting-note">
                <strong>⚠️ Posting Requirements:</strong><br>
                • Use <strong>Tor Browser</strong> or configure SOCKS5 proxy (127.0.0.1:9050)<br>
                • Link opens in new tab to posting service<br>
                • Anonymous posting available on news.tcpreset.net
            </p>
        </div>

        {{if .ConnectionError}}
        <div class="error">
            <h3>❌ Connection Error</h3>
            <p>{{.ConnectionError}}</p>
            <p>Showing cached information only. <a href="/health" style="color: #00aaff;">Check system health</a></p>
        </div>
        {{end}}

        {{if .Threads}}
        <div class="thread-list">
            <h3>🧵 Discussion Threads {{if .TruncatedView}}(showing recent {{.MaxArticles}} articles organized into threads){{end}}</h3>
            
            {{range .Threads}}
            <div class="thread-item">
                <div class="thread-subject">
                    <a href="/article/{{$.GroupName}}/{{.Root.Article.Number}}">{{.Subject}}</a>
                </div>
                <div class="thread-meta">
                    <strong>Started by:</strong> {{.Root.Article.From}} • 
                    <strong>Articles:</strong> {{.ArticleCount}} • 
                    <strong>Last activity:</strong> {{.LastDate.Format "Jan 2, 2006 15:04"}}
                </div>
                
                {{if .Root.Children}}
                <div class="thread-replies">
                    {{range .Root.Children}}
                    <div class="reply-item">
                        <div class="reply-subject">
                            <a href="/article/{{$.GroupName}}/{{.Article.Number}}">{{.Article.Subject}}</a>
                        </div>
                        <div class="reply-meta">
                            By {{.Article.From}} • {{.Article.ParsedDate.Format "Jan 2, 15:04"}}
                        </div>
                        
                        {{if .Children}}
                        {{range .Children}}
                        <div class="reply-item" style="margin-left: 15px; border-left: 1px solid #555; padding-left: 10px;">
                            <div class="reply-subject">
                                <a href="/article/{{$.GroupName}}/{{.Article.Number}}">{{.Article.Subject}}</a>
                            </div>
                            <div class="reply-meta">
                                By {{.Article.From}} • {{.Article.ParsedDate.Format "Jan 2, 15:04"}}
                            </div>
                        </div>
                        {{end}}
                        {{end}}
                    </div>
                    {{end}}
                </div>
                {{end}}
            </div>
            {{end}}
            
            {{if .TruncatedView}}
            <div class="info">
                <p><strong>🧵 Threading Info:</strong> Articles are automatically organized into conversation threads using References headers and subject matching.</p>
                <p><strong>ℹ️ Note:</strong> Only showing the most recent {{.MaxArticles}} articles to prevent slow loading.</p>
                <p>Use a dedicated newsreader client for full group browsing and threading.</p>
            </div>
            {{end}}
        </div>
        {{else if not .ConnectionError}}
        <div class="warning">
            <h3>⚠️ No Articles Available</h3>
            <p>This group appears to be empty or articles couldn't be retrieved.</p>
            <p>This could be normal for:</p>
            <ul>
                <li>Newly created groups</li>
                <li>Low-traffic groups</li>
                <li>Groups with expired articles</li>
            </ul>
            <button onclick="window.location.reload()" class="refresh-btn">🔄 Refresh</button>
        </div>
        {{end}}

        <div class="info">
            <h3>💡 Navigation Tips</h3>
            <ul>
                <li><strong>🧵 Threaded view:</strong> Articles are grouped by conversation thread</li>
                <li><strong>Click thread subjects</strong> to read the original post</li>
                <li><strong>Click reply subjects</strong> to read individual replies</li>
                <li><strong>Indentation shows</strong> the reply hierarchy</li>
                <li><strong>Recent threads first</strong> based on last activity</li>
            </ul>
        </div>
        
        <footer class="app-footer">
            <div class="footer-links">
                <a href="https://github.com/gabrix73/onion-newsreader.git" target="_blank" rel="noopener">📁 Source Code</a>
                <a href="monero:44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L" onclick="fallbackCopyXMR(event)">⚡ Support Project</a>
                <a href="https://www.virebent.art" target="_blank" rel="noopener">🌐 virebent.art</a>
            </div>
        </footer>
    </div>
    
    <script>
    function fallbackCopyXMR(event) {
        const xmrAddress = "44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L";
        
        if (!navigator.userAgent.includes('Monero')) {
            event.preventDefault();
            if (navigator.clipboard && navigator.clipboard.writeText) {
                navigator.clipboard.writeText(xmrAddress).then(() => {
                    alert("✅ XMR address copied to clipboard!");
                }).catch(() => {
                    prompt("Copy XMR address:", xmrAddress);
                });
            } else {
                prompt("Copy XMR address:", xmrAddress);
            }
        }
    }
    </script>
</body>
</html>`

	tmpl, _ := template.New("group").Parse(groupHTML)
	
	// Generate search pattern for similar groups
	groupPattern := ""
	if strings.Contains(groupName, ".") {
		parts := strings.Split(groupName, ".")
		if len(parts) > 1 {
			groupPattern = parts[0] + ".*"
		}
	}
	
	data := map[string]interface{}{
		"GroupName":         groupName,
		"GroupPattern":      groupPattern,
		"CachedLow":         groupInfo.Low,
		"CachedHigh":        groupInfo.High, 
		"CachedCount":       groupInfo.High - groupInfo.Low + 1,
		"Status":            groupInfo.Status,
		"Threads":           threads,
		"ThreadCount":       len(threads),
		"ConnectionError":   connectionError,
		"LiveStats":         connectionError == "",
		"MaxArticles":       200,
		"TruncatedView":     len(threads) > 0,
		"EncodedGroupName":  url.QueryEscape(groupName),
	}
	
	if connectionError == "" {
		data["LiveLow"] = low
		data["LiveHigh"] = high
		data["LiveCount"] = articleCount
	}
	
	tmpl.Execute(w, data)
}

func (s *NewsServer) handleArticle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	
	// Extract group name and article number from URL path
	path := strings.TrimPrefix(r.URL.Path, "/article/")
	parts := strings.Split(path, "/")
	
	if len(parts) != 2 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	
	groupName := parts[0]
	articleNumber, err := strconv.Atoi(parts[1])
	if err != nil {
		http.Redirect(w, r, "/group/"+groupName, http.StatusSeeOther)
		return
	}
	
	log.Printf("📰 Reading article %d from group %s", articleNumber, groupName)
	
	// Ensure we're connected and select the group first
	var headers map[string]string
	var body []string
	var connectionError string
	
	if err := s.client.ensureConnected(); err != nil {
		connectionError = fmt.Sprintf("Connection failed: %v", err)
	} else {
		// Select group first
		_, _, _, err := s.client.SelectGroup(groupName)
		if err != nil {
			connectionError = fmt.Sprintf("Failed to select group: %v", err)
		} else {
			// Get the article
			headers, body, err = s.client.GetArticle(groupName, articleNumber)
			if err != nil {
				connectionError = fmt.Sprintf("Failed to get article: %v", err)
			}
		}
	}
	
	articleHTML := `<!DOCTYPE html>
<html>
<head>
    <title>📰 Article {{.ArticleNumber}} - {{.GroupName}} - Onion Newsreader</title>
    <meta charset="utf-8">
    <style>
        body { font-family: 'Monaco', 'Menlo', 'Ubuntu Mono', monospace; background: #0a0a0a; color: #00ff00; padding: 20px; line-height: 1.4; }
        .container { max-width: 1000px; margin: 0 auto; }
        .header { background: #1a1a1a; padding: 20px; border-radius: 8px; margin-bottom: 20px; border: 1px solid #333; }
        .article-headers { background: #1a1a1a; padding: 15px; border-radius: 8px; margin-bottom: 20px; border: 1px solid #333; }
        .article-body { background: #1a1a1a; padding: 20px; border-radius: 8px; border: 1px solid #333; }
        .header-item { margin: 5px 0; padding: 5px; background: #0a0a0a; border-radius: 3px; }
        .header-label { color: #ffaa00; font-weight: bold; }
        .header-value { color: #00aaff; word-break: break-word; }
        .body-content { 
            color: #cccccc; 
            white-space: pre-wrap; 
            font-family: 'Monaco', 'Menlo', 'Ubuntu Mono', monospace; 
            background: #0a0a0a; 
            padding: 15px; 
            border-radius: 5px; 
            border: 1px solid #333;
            overflow-x: auto;
            line-height: 1.4;
            word-wrap: break-word;
        }
        .back-link { color: #00aaff; text-decoration: none; }
        .back-link:hover { color: #00ff00; }
        .error { background: #2a1a1a; border: 1px solid #aa0000; color: #aa0000; padding: 15px; border-radius: 8px; margin: 20px 0; }
        .warning { background: #2a1a00; border: 1px solid #ffaa00; color: #ffaa00; padding: 15px; border-radius: 8px; margin: 20px 0; }
        .info { background: #1a2a2a; border: 1px solid #0088aa; color: #0088aa; padding: 15px; border-radius: 8px; margin: 20px 0; }
        .article-meta { display: grid; grid-template-columns: repeat(auto-fit, minmax(300px, 1fr)); gap: 10px; }
        .subject-highlight { font-size: 1.1em; color: #00ff00; font-weight: bold; }
        .quoted-text { 
            color: #888888; 
            font-style: italic;
            border-left: 2px solid #666;
            padding-left: 8px;
            margin-left: 4px;
            display: block;
        }
        .signature { 
            color: #666666; 
            border-top: 1px solid #333; 
            margin-top: 10px; 
            padding-top: 10px; 
            font-size: 0.9em;
            display: block;
        }
        .url-link {
            color: #00aaff;
            text-decoration: underline;
        }
        .reply-section { 
            background: #1a1a1a; 
            padding: 20px; 
            border-radius: 8px; 
            margin-bottom: 20px; 
            border: 1px solid #333; 
            text-align: center; 
        }
        .reply-button { 
            display: inline-block; 
            background: #2a2a4a; 
            color: #00aaff; 
            text-decoration: none; 
            padding: 12px 24px; 
            border-radius: 6px; 
            font-weight: bold; 
            margin: 10px 0; 
            border: 2px solid #0088aa;
            transition: all 0.3s ease;
        }
        .reply-button:hover { 
            background: #3a3a6a; 
            border-color: #00aaff; 
            box-shadow: 0 0 10px rgba(0, 170, 255, 0.3);
        }
        .reply-note { 
            font-size: 0.9em; 
            color: #888; 
            margin-top: 15px; 
            text-align: left; 
            background: #0a0a0a; 
            padding: 10px; 
            border-radius: 4px; 
            border-left: 3px solid #00aaff;
        }
        .app-footer {
            margin-top: 40px;
            padding: 25px 0;
            border-top: 2px solid #333;
            background: #1a1a1a;
            text-align: center;
        }
        .footer-links {
            display: flex;
            justify-content: center;
            gap: 25px;
            flex-wrap: wrap;
        }
        .footer-links a {
            color: #00aaff;
            text-decoration: none;
            font-weight: 500;
            padding: 8px 15px;
            border-radius: 20px;
            background: rgba(0, 170, 255, 0.1);
            transition: all 0.3s ease;
            border: 1px solid rgba(0, 170, 255, 0.2);
        }
        .footer-links a:hover {
            background: #00aaff;
            color: #0a0a0a;
        }
        .footer-links a[href^="monero:"] {
            background: rgba(255, 102, 0, 0.1);
            color: #ff6600;
            border-color: rgba(255, 102, 0, 0.2);
        }
        .footer-links a[href^="monero:"]:hover {
            background: #ff6600;
            color: #0a0a0a;
        }
        .footer-links a[href*="github"] {
            background: rgba(51, 51, 51, 0.1);
            color: #00ff00;
            border-color: rgba(51, 51, 51, 0.2);
        }
        .footer-links a[href*="github"]:hover {
            background: #00ff00;
            color: #0a0a0a;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>📰 Article {{.ArticleNumber}}</h1>
            <p><a href="/" class="back-link">← Back to Home</a> | 
               <a href="/group/{{.GroupName}}" class="back-link">← Back to {{.GroupName}}</a> | 
               <a href="/active" class="back-link">📋 All Groups</a></p>
        </div>

        {{if not .ConnectionError}}
        <div class="reply-section">
            <h3>💬 Reply to this Article</h3>
            <p>Reply to article by <strong>{{.From}}</strong> in <strong>{{.GroupName}}</strong></p>
            <a href="http://itcxzfm2h36hfj6j7qxksyfm4ipp3co4rkl62sgge7hp6u77lbretiyd.onion:8880/#send{{if .EncodedGroupName}}?newsgroups={{.EncodedGroupName}}{{end}}{{if .EncodedSubject}}&subject={{.EncodedSubject}}{{end}}{{if .EncodedMessageID}}&references={{.EncodedMessageID}}{{end}}{{if .EncodedQuotedContent}}&message={{.EncodedQuotedContent}}{{end}}" target="_blank" class="reply-button">
                💬 REPLY TO POST
            </a>
            <p class="reply-note">
                <strong>ℹ️ Reply Pre-filled Data:</strong><br>
                {{if .Subject}}• <strong>Subject:</strong> Re: {{.Subject}}<br>{{end}}
                {{if .MessageID}}• <strong>References:</strong> {{.MessageID}}<br>{{end}}
                • <strong>Newsgroups:</strong> {{.GroupName}}<br>
                • <strong>Message:</strong> Quoted original content included<br>
                • <strong>Direct to:</strong> Send Message tab with pre-filled fields
            </p>
        </div>
        {{end}}

        {{if .ConnectionError}}
        <div class="error">
            <h3>❌ Connection Error</h3>
            <p>{{.ConnectionError}}</p>
            <p><a href="/health" style="color: #00aaff;">Check system health</a> | 
               <a href="/group/{{.GroupName}}" style="color: #00aaff;">Back to group</a></p>
        </div>
        {{else}}
        
        <div class="article-headers">
            <h3>📋 Article Headers</h3>
            <div class="article-meta">
                {{if .Subject}}
                <div class="header-item">
                    <span class="header-label">Subject:</span><br>
                    <span class="subject-highlight">{{.Subject}}</span>
                </div>
                {{end}}
                
                {{if .From}}
                <div class="header-item">
                    <span class="header-label">From:</span><br>
                    <span class="header-value">{{.From}}</span>
                </div>
                {{end}}
                
                {{if .Date}}
                <div class="header-item">
                    <span class="header-label">Date:</span><br>
                    <span class="header-value">{{.Date}}</span>
                </div>
                {{end}}
                
                {{if .MessageID}}
                <div class="header-item">
                    <span class="header-label">Message-ID:</span><br>
                    <span class="header-value">{{.MessageID}}</span>
                </div>
                {{end}}
                
                {{if .Newsgroups}}
                <div class="header-item">
                    <span class="header-label">Newsgroups:</span><br>
                    <span class="header-value">{{.Newsgroups}}</span>
                </div>
                {{end}}
                
                {{if .References}}
                <div class="header-item">
                    <span class="header-label">References:</span><br>
                    <span class="header-value" style="font-size: 0.8em; word-break: break-all;">{{.References}}</span>
                </div>
                {{end}}
            </div>
            
            {{if .AllHeaders}}
            <details style="margin-top: 15px;">
                <summary style="color: #ffaa00; cursor: pointer;">🔍 Show All Headers</summary>
                <div style="margin-top: 10px; background: #0a0a0a; padding: 10px; border-radius: 5px;">
                    {{range $key, $value := .AllHeaders}}
                    <div style="margin: 2px 0; font-size: 0.9em;">
                        <strong style="color: #ffaa00;">{{$key}}:</strong> 
                        <span style="color: #cccccc; word-break: break-word;">{{$value}}</span>
                    </div>
                    {{end}}
                </div>
            </details>
            {{end}}
        </div>

        <div class="article-body">
            <h3>📄 Article Content</h3>
            {{if .Body}}
            <div class="body-content">{{.Body}}</div>
            {{else}}
            <div class="warning">
                <p>⚠️ Article body is empty or could not be retrieved.</p>
            </div>
            {{end}}
        </div>
        
        <div class="info">
            <h3>💡 Navigation Tips</h3>
            <ul>
                <li><strong>Use browser back button</strong> to return to threaded group view</li>
                <li><strong>References header</strong> shows message threading</li>
                <li><strong>Message-ID</strong> is unique identifier for this article</li>
                <li><strong>Quoted text</strong> appears dimmed and indented</li>
                <li><strong>Signatures</strong> appear separated and dimmed</li>
                <li><strong>Threading</strong> organizes related posts in conversation flow</li>
            </ul>
        </div>
        
        {{end}}
        
        <footer class="app-footer">
            <div class="footer-links">
                <a href="https://github.com/gabrix73/onion-newsreader.git" target="_blank" rel="noopener">📁 Source Code</a>
                <a href="monero:44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L" onclick="fallbackCopyXMR(event)">⚡ Support Project</a>
                <a href="https://www.virebent.art" target="_blank" rel="noopener">🌐 virebent.art</a>
            </div>
        </footer>
    </div>
    
    <script>
    function fallbackCopyXMR(event) {
        const xmrAddress = "44L7s3EK6WngbNEZUKBeHwWWTgafpYX98MDaz2LSxkTHWtqPcnhiqzgXC9rjb45VHbWeesdgp2tcR9y5ApegoszyNMemz4L";
        
        if (!navigator.userAgent.includes('Monero')) {
            event.preventDefault();
            if (navigator.clipboard && navigator.clipboard.writeText) {
                navigator.clipboard.writeText(xmrAddress).then(() => {
                    alert("✅ XMR address copied to clipboard!");
                }).catch(() => {
                    prompt("Copy XMR address:", xmrAddress);
                });
            } else {
                prompt("Copy XMR address:", xmrAddress);
            }
        }
    }
    </script>
</body>
</html>`

	tmpl, _ := template.New("article").Parse(articleHTML)
	
	// Process body for better display with proper HTML escaping
	var bodyHTML template.HTML
	var quotedBody string // For reply functionality
	if len(body) > 0 {
		bodyText := strings.Join(body, "\n")
		
		// Create quoted version for replies (add > to each line)
		quotedLines := strings.Split(bodyText, "\n")
		for i, line := range quotedLines {
			quotedLines[i] = "> " + line
		}
		quotedBody = strings.Join(quotedLines, "\n")
		
		// Prima escape tutto l'HTML per sicurezza
		bodyText = html.EscapeString(bodyText)
		
		// Poi aggiungi styling per quotes e signature
		lines := strings.Split(bodyText, "\n")
		inSignature := false
		
		for i, line := range lines {
			trimmedLine := strings.TrimSpace(line)
			
			// Detect signature start (-- line)
			if trimmedLine == "--" && !inSignature {
				inSignature = true
				lines[i] = `<div class="signature">` + line
				continue
			}
			
			// Se siamo in signature, continua lo styling
			if inSignature {
				if i == len(lines)-1 {
					lines[i] = line + `</div>` // Chiudi signature alla fine
				}
				continue
			}
			
			// Quote detection (linee che iniziano con >)
			if strings.HasPrefix(trimmedLine, "&gt;") {
				lines[i] = `<div class="quoted-text">` + line + `</div>`
			}
			
			// URL detection semplice
			if strings.Contains(line, "http://") || strings.Contains(line, "https://") || strings.Contains(line, "ftp://") {
				// Non implementiamo link cliccabili per sicurezza, ma evidenziamo
				lines[i] = strings.ReplaceAll(line, "http://", `<span class="url-link">http://`) 
				lines[i] = strings.ReplaceAll(lines[i], "https://", `<span class="url-link">https://`)
				lines[i] = strings.ReplaceAll(lines[i], "ftp://", `<span class="url-link">ftp://`)
				// Chiudi span alla fine della linea se contiene URL
				if strings.Contains(lines[i], `<span class="url-link">`) {
					lines[i] += `</span>`
				}
			}
		}
		
		// Se signature non è stata chiusa, chiudila
		if inSignature && !strings.Contains(lines[len(lines)-1], "</div>") {
			lines[len(lines)-1] += `</div>`
		}
		
		processedBody := strings.Join(lines, "\n")
		
		// Marca come HTML sicuro
		bodyHTML = template.HTML(processedBody)
	}
	
	data := map[string]interface{}{
		"GroupName":         groupName,
		"ArticleNumber":     articleNumber,
		"ConnectionError":   connectionError,
		"Body":             bodyHTML,  // Usa bodyHTML invece di bodyText
		"AllHeaders":       headers,
		"EncodedGroupName": url.QueryEscape(groupName),
		"QuotedBody":       quotedBody,
	}
	
	// Extract common headers for easy access and URL encode them
	if headers != nil {
		if subject, ok := headers["Subject"]; ok {
			data["Subject"] = subject
			// Add "Re: " if not already present for reply functionality
			replySubject := subject
			if !strings.HasPrefix(strings.ToLower(subject), "re:") {
				replySubject = "Re: " + subject
			}
			data["EncodedSubject"] = url.QueryEscape(replySubject)
		}
		if from, ok := headers["From"]; ok {
			data["From"] = from
		}
		if date, ok := headers["Date"]; ok {
			data["Date"] = date
		}
		if msgID, ok := headers["Message-ID"]; ok {
			data["MessageID"] = msgID
			// Ensure Message-ID has proper < > format for References
			properMsgID := msgID
			if !strings.HasPrefix(msgID, "<") {
				properMsgID = "<" + msgID
			}
			if !strings.HasSuffix(properMsgID, ">") {
				properMsgID = properMsgID + ">"
			}
			data["EncodedMessageID"] = url.QueryEscape(properMsgID)
		}
		if newsgroups, ok := headers["Newsgroups"]; ok {
			data["Newsgroups"] = newsgroups
		}
		if references, ok := headers["References"]; ok {
			data["References"] = references
		}
	}
	
	// Prepare quoted content for reply
	if quotedBody != "" && headers != nil {
		if from, ok := headers["From"]; ok {
			if date, ok := headers["Date"]; ok {
				quotedContent := fmt.Sprintf("On %s, %s wrote:\n\n%s", date, from, quotedBody)
				data["EncodedQuotedContent"] = url.QueryEscape(quotedContent)
			}
		}
	}
	
	tmpl.Execute(w, data)
}

func main() {
	log.Printf("📝 Logging initialized - writing to stdout and %s", "/var/log/onion-newsreader/onion-newsreader.log")
	log.Printf("🧅 Onion Newsreader starting with threading support...")

	server := NewNewsServer()
	defer server.client.Close()

	log.Printf("🚀 Starting Onion Newsreader HTTP server...")

	http.HandleFunc("/", server.handleHome)
	http.HandleFunc("/health", server.handleHealth)
	http.HandleFunc("/test-onion", server.handleTestOnion)
	http.HandleFunc("/search", server.handleSearch)
	http.HandleFunc("/active", server.handleActive)
	http.HandleFunc("/download-status", server.handleDownloadStatus)
	http.HandleFunc("/group/", server.handleGroup)
	http.HandleFunc("/article/", server.handleArticle)

	log.Printf("✅ HTTP server initialization complete")
	log.Printf("🔄 Starting background NNTP connection routine...")

	go func() {
		for {
			time.Sleep(10 * time.Second)
			log.Printf("🔄 Attempting NNTP connection...")
			if err := server.client.ensureConnected(); err != nil {
				log.Printf("⚠️ NNTP connection failed: %v", err)
				log.Printf("⏰ Retrying NNTP connection in 120 seconds...")
				time.Sleep(2 * time.Minute)
			} else {
				log.Printf("✅ NNTP connection established successfully")
				break
			}
		}
	}()

	log.Printf("🚀 Starting HTTP server on 127.0.0.1:8080")
	log.Printf("📡 Target NNTP: peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion:119")
	log.Printf("🔄 Fallback NNTP: news.tcpreset.net:119 (anonymous access)")
	log.Printf("✍️ Posting URL: http://itcxzfm2h36hfj6j7qxksyfm4ipp3co4rkl62sgge7hp6u77lbretiyd.onion:8880/")
	log.Printf("🧵 Threading: Enabled (References + Subject-based)")
	log.Printf("🔒 Stealth mode: true")
	log.Printf("💬 Support contact: info@tcpreset.net")
	log.Printf("🌐 Web interface ready - NNTP connection will be established in background")

	if err := http.ListenAndServe("127.0.0.1:8080", nil); err != nil {
		log.Fatalf("❌ HTTP server failed: %v", err)
	}
}
